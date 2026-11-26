package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"atropos-go/loadgen/internal/sse"
	"atropos-go/loadgen/internal/stats"
)

// LauncherConfig configures the k6 subprocess launcher.
type LauncherConfig struct {
	// K6Bin is the k6 executable. Default "k6" (resolved via PATH).
	K6Bin string
	// K6Dir is the directory holding runner.js, scripts/, personas/ and
	// flows/. The launcher stages each run's workflow document under
	// K6Dir/flows/zeus-runs/<run_id>.json (runner.js can only open() files
	// relative to itself), so flows/ must be writable.
	K6Dir string
	// DatasetURL maps a dataset id to the HTTP endpoint the k6 engine
	// fetches pool data from (ZEUS_DATASET_ENDPOINT). Required only for
	// runs that reference a dataset.
	DatasetURL func(datasetID string) string
	// MaxRunDuration is the supervision deadline for runs whose spec
	// carries no duration; runs with a DurationS get DurationS + 60s of
	// grace instead. Default 30m.
	MaxRunDuration time.Duration
	// SnapshotInterval is how often an in-flight run's interim stats
	// snapshot is refreshed in the snapshot store and published as a
	// stats.snapshot SSE event. Default 2s (the contract promises <= 5s).
	SnapshotInterval time.Duration
	Logger           *slog.Logger
}

// sampleDrainTimeout bounds how long supervision waits, after k6 exits, for
// the sample reader to drain k6's last flushed output.
const sampleDrainTimeout = 10 * time.Second

// LaunchSpec carries the per-run execution parameters (from the create-run
// request plus the workflow document).
type LaunchSpec struct {
	VUs int
	// RateRPS is the offered iteration arrival rate for the open-loop
	// generator. When > 0 it rides to k6 as -e RATE and overrides the legacy
	// vus * estimated_rps_per_vu derivation, decoupling the rate from the VU
	// pool (gate G2): vus then means concurrency budget only.
	RateRPS       float64
	DurationS     int
	Persona       string // default "cautious" (a file under K6Dir/personas)
	BaseURL       string // workflow base_url; empty = flow embeds absolute URLs
	DatasetID     string // empty = no dataset (an empty pool map is staged)
	WorkflowLabel string // rides W3C baggage as atropos.workflow
	MetaTraceID   string // rides W3C baggage as meta-trace-id (the id the create-run response echoed)
}

// Launcher supervises one k6 subprocess per run and is the ONLY writer of a
// run's post-create lifecycle: it advances starting -> validating -> running
// -> completing -> completed/failed and publishes run.state events on the
// SSE broker. Stop kills the subprocess; the DELETE handler owns the
// 'stopped' status (the launcher never overwrites a terminal status --
// UpdateStatus enforces the FSM).
//
// Stats: k6 streams every metric sample as JSON lines into a per-run named
// pipe (--out json=<pipe>). A reader goroutine aggregates them on the fly
// (stats.LiveRun: running totals + a fixed-size latency sketch, nothing
// retained) and feeds the per-run Prometheus series. Every SnapshotInterval
// the interim totals land in the snapshot store and go out as a
// stats.snapshot event. When the run ends, the final snapshot comes from
// k6's --summary-export if k6 wrote one (completed runs), else from the
// stream totals gathered up to the end (stopped and failed runs).
type Launcher struct {
	cfg       LauncherConfig
	runs      *Store
	snapshots *stats.SnapshotStore
	broker    *sse.Broker
	metrics   *stats.Metrics

	mu     sync.Mutex
	cancel map[string]context.CancelFunc
	wg     sync.WaitGroup
}

// NewLauncher wires a Launcher. snapshots, broker and metrics may be nil (no
// stats / no events / no Prometheus feed); runs is required.
func NewLauncher(cfg LauncherConfig, runs *Store, snapshots *stats.SnapshotStore, broker *sse.Broker, metrics *stats.Metrics) *Launcher {
	if cfg.K6Bin == "" {
		cfg.K6Bin = "k6"
	}
	if cfg.K6Dir == "" {
		cfg.K6Dir = "./k6"
	}
	if cfg.MaxRunDuration <= 0 {
		cfg.MaxRunDuration = 30 * time.Minute
	}
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = 2 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Launcher{cfg: cfg, runs: runs, snapshots: snapshots, broker: broker, metrics: metrics, cancel: map[string]context.CancelFunc{}}
}

// Launch stages the workflow document and starts the k6 subprocess for rn.
// It returns once the process has started (or failed to); the run is then
// owned by the supervision goroutine until a terminal state. flowDoc is the
// workflow's original registered JSON document.
func (l *Launcher) Launch(rn *Run, flowDoc []byte, spec LaunchSpec) error {
	flowPath, err := l.stageFlow(rn.ID, flowDoc)
	if err != nil {
		l.fail(rn.ID, "stage flow: "+err.Error())
		return err
	}

	files := runFiles{
		flow:    flowPath,
		summary: filepath.Join(os.TempDir(), "zeus-run-"+rn.ID+"-summary.json"),
		samples: filepath.Join(os.TempDir(), "zeus-run-"+rn.ID+"-samples.pipe"),
	}
	_ = os.Remove(files.samples) // stale pipe from a crashed predecessor
	sampleR, sampleHold, err := openSamplePipe(files.samples)
	streaming := err == nil
	if !streaming {
		l.cfg.Logger.Warn("launcher: no live stats for run", "run_id", rn.ID, "error", err)
	}
	closePipe := func() {
		if streaming {
			_ = sampleR.Close()
			_ = sampleHold.Close()
		}
	}

	args := []string{
		"run",
		"--quiet",
		"--summary-export", files.summary,
		// p(99) is not in k6's default trend stats; RunStats wants it.
		"--summary-trend-stats", "avg,min,med,max,p(90),p(95),p(99)",
		"-e", "FLOW=zeus-runs/" + rn.ID,
		"-e", "ZEUS_RUN_ID=" + rn.ID,
	}
	if streaming {
		// k6's built-in JSON output: one sample per line, flushed every
		// 200ms, written straight to the pipe (no k6-side buffering).
		args = append(args, "--out", "json="+files.samples)
	}
	if spec.Persona == "" {
		spec.Persona = "cautious"
	}
	args = append(args, "-e", "PERSONA="+spec.Persona)
	if spec.WorkflowLabel != "" {
		args = append(args, "-e", "ZEUS_WORKFLOW_LABEL="+spec.WorkflowLabel)
	}
	if spec.MetaTraceID != "" {
		args = append(args, "-e", "ZEUS_META_TRACE_ID="+spec.MetaTraceID)
	}
	if spec.BaseURL != "" {
		args = append(args, "-e", "BASE_URL="+spec.BaseURL)
	}
	if spec.VUs > 0 {
		args = append(args, "-e", "VUS="+strconv.Itoa(spec.VUs))
	}
	if spec.RateRPS > 0 {
		args = append(args, "-e", "RATE="+strconv.FormatFloat(spec.RateRPS, 'f', -1, 64))
	}
	if spec.DurationS > 0 {
		args = append(args, "-e", "DURATION="+strconv.Itoa(spec.DurationS)+"s")
	}
	if spec.DatasetID != "" && l.cfg.DatasetURL != nil {
		args = append(args, "-e", "ZEUS_DATASET_ENDPOINT="+l.cfg.DatasetURL(spec.DatasetID))
	}
	args = append(args, "runner.js")

	// Supervision deadline: DURATION is only advisory inside k6, so a wedged
	// process would otherwise hold the run in 'running' forever and stall
	// the control plane's phase.
	timeout := l.cfg.MaxRunDuration
	if spec.DurationS > 0 {
		timeout = time.Duration(spec.DurationS)*time.Second + 60*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, l.cfg.K6Bin, args...)
	cmd.Dir = l.cfg.K6Dir
	tail := newTailBuffer(16 << 10)
	cmd.Stdout = tail
	cmd.Stderr = tail

	// validating = "we are about to hand the document to the engine"; k6's
	// init context re-parses it and a broken doc dies before any load.
	l.transition(rn.ID, StatusValidating)

	if err := cmd.Start(); err != nil {
		cancel()
		closePipe()
		files.remove()
		l.fail(rn.ID, "k6 start: "+err.Error())
		return err
	}

	startedAt := rn.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	live := stats.NewLiveRun(l.metrics, rn.ID, rn.WorkflowID, startedAt)
	readerDone := make(chan struct{})
	if streaming {
		go l.readSamples(rn.ID, sampleR, live, readerDone)
	} else {
		close(readerDone)
	}

	l.mu.Lock()
	l.cancel[rn.ID] = cancel
	l.mu.Unlock()

	if l.transition(rn.ID, StatusRunning) {
		// An all-zero interim snapshot right away, so /stats reports an
		// in-flight run as such before the first tick.
		l.publishSnapshot(live.Snapshot(StatusRunning, false))
	}
	tickStop, tickDone := make(chan struct{}), make(chan struct{})
	go l.snapshotLoop(rn.ID, live, tickStop, tickDone)

	l.wg.Add(1)
	go l.supervise(ctx, cmd, rn.ID, files, tail, live, sampleHold, readerDone, func() {
		close(tickStop)
		<-tickDone
	})
	return nil
}

// runFiles are the per-run scratch paths the launcher owns.
type runFiles struct {
	flow    string // staged workflow document
	summary string // k6 --summary-export target
	samples string // named pipe k6's JSON output streams into
}

func (f runFiles) remove() {
	_ = os.Remove(f.flow)
	_ = os.Remove(f.summary)
	_ = os.Remove(f.samples)
}

// readSamples aggregates the run's k6 sample stream until EOF: k6 has
// exited and supervise has closed the placeholder write end.
func (l *Launcher) readSamples(runID string, f *os.File, live *stats.LiveRun, done chan<- struct{}) {
	defer close(done)
	defer f.Close()
	bad, err := streamSamples(f, live)
	if err != nil {
		l.cfg.Logger.Warn("launcher: read sample stream", "run_id", runID, "error", err)
	}
	if bad > 0 {
		l.cfg.Logger.Warn("launcher: skipped undecodable k6 sample lines", "run_id", runID, "lines", bad)
	}
}

// snapshotLoop refreshes the run's interim snapshot every SnapshotInterval
// until stop is closed. Terminal runs are skipped: supervision owns the
// final snapshot.
func (l *Launcher) snapshotLoop(runID string, live *stats.LiveRun, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(l.cfg.SnapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			rn, ok := l.runs.Get(runID)
			if !ok || IsTerminal(rn.Status) {
				continue
			}
			l.publishSnapshot(live.Snapshot(rn.Status, false))
		}
	}
}

// Stop kills the run's k6 subprocess, if one is live, and reports whether
// it was. The caller (the DELETE handler) owns the 'stopped' status
// transition; Stop only signals. When Stop returns true, supervision saves
// the stopped run's final stats snapshot and then closes its SSE stream.
// Unknown ids are a no-op (false).
func (l *Launcher) Stop(runID string) bool {
	l.mu.Lock()
	cancel := l.cancel[runID]
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

// Close kills every live subprocess and waits for supervision to drain.
func (l *Launcher) Close() {
	l.mu.Lock()
	for _, cancel := range l.cancel {
		cancel()
	}
	l.mu.Unlock()
	l.wg.Wait()
}

// supervise waits for the k6 process, drains its sample stream, finalizes
// the run's status and final stats snapshot, and closes its SSE stream.
func (l *Launcher) supervise(ctx context.Context, cmd *exec.Cmd, runID string, files runFiles, tail *tailBuffer,
	live *stats.LiveRun, sampleHold *os.File, readerDone <-chan struct{}, stopTicks func()) {
	defer l.wg.Done()
	err := cmd.Wait()

	l.mu.Lock()
	cancel := l.cancel[runID]
	delete(l.cancel, runID)
	l.mu.Unlock()

	// k6 is gone: drop zeus's placeholder write end so the reader drains
	// whatever k6 flushed before exiting and then sees EOF; then stop the
	// interim ticker so it cannot overwrite the final snapshot.
	if sampleHold != nil {
		_ = sampleHold.Close()
	}
	select {
	case <-readerDone:
	case <-time.After(sampleDrainTimeout):
		l.cfg.Logger.Warn("launcher: sample stream did not drain; final stream stats may be short", "run_id", runID)
	}
	stopTicks()
	live.End()

	defer func() {
		if cancel != nil {
			cancel() // release the deadline timer (runs after the ctx.Err() branching)
		}
		files.remove()
		if l.broker != nil {
			l.broker.CloseRun(runID)
		}
	}()

	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The watchdog killed a wedged or overrunning k6 -- a
			// launcher-owned failure, not an operator stop.
			if l.fail(runID, "run deadline exceeded") {
				l.countEnded(runID, StatusFailed)
			}
			l.saveFinal(runID, live.Snapshot(StatusFailed, true))
			return
		}
		// Killed via Stop/Close: the stop path owns the terminal status
		// ('stopped'); keep the stats gathered up to the kill and report
		// what happened.
		l.saveFinal(runID, live.Snapshot(StatusStopped, true))
		l.publishState(runID, StatusStopped, "killed")
		return
	}

	if err != nil {
		// Exit 99 = thresholds crossed: k6 ran the load to completion and
		// wrote the full summary export; a degraded target is the
		// measurement, not a run failure. Record the breach and fall
		// through to the completion path so the stats survive.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 99 {
			reason := fmt.Sprintf("k6 exited: %v; tail: %s", err, tail.String())
			if l.fail(runID, reason) {
				l.countEnded(runID, StatusFailed)
			}
			l.saveFinal(runID, live.Snapshot(StatusFailed, true))
			return
		}
		if uerr := l.runs.UpdateReason(runID, "thresholds breached"); uerr != nil {
			l.cfg.Logger.Warn("launcher: persist breach reason", "run_id", runID, "error", uerr)
		}
	}

	l.transition(runID, StatusCompleting)
	rs := l.parseSummary(runID, files.summary)
	if rs == nil {
		// No usable summary: keep what the stream saw rather than nothing.
		rs = live.Snapshot(StatusCompleted, true)
	}
	l.saveFinal(runID, rs)
	if l.transition(runID, StatusCompleted) {
		l.countEnded(runID, StatusCompleted)
	}
}

// publishSnapshot stores rs as the run's current snapshot and emits it as a
// stats.snapshot event.
func (l *Launcher) publishSnapshot(rs *stats.RunStats) {
	if l.snapshots != nil {
		l.snapshots.Save(rs)
	}
	if l.broker != nil {
		payload, err := json.Marshal(rs)
		if err != nil {
			return
		}
		l.broker.Publish(rs.RunID, sse.Event{Type: "stats.snapshot", Data: string(payload), RunID: rs.RunID})
	}
}

// saveFinal publishes a run's final snapshot. If the run already holds a
// terminal status (e.g. the operator stopped it while k6 was finishing),
// the snapshot reports that status.
func (l *Launcher) saveFinal(runID string, rs *stats.RunStats) {
	if rn, ok := l.runs.Get(runID); ok && IsTerminal(rn.Status) {
		rs.Status = rn.Status
	}
	l.publishSnapshot(rs)
}

// countEnded records a launcher-finalized run (completed or failed) in the
// run counters. Stopped runs are counted by the DELETE handler.
func (l *Launcher) countEnded(runID, status string) {
	if l.metrics == nil {
		return
	}
	rn, ok := l.runs.Get(runID)
	if !ok {
		return
	}
	l.metrics.ActiveRuns.WithLabelValues().Dec()
	l.metrics.RunsCompletedTotal.WithLabelValues(rn.WorkflowID, rn.WorkflowName, status).Inc()
}

// stageFlow writes the workflow document where runner.js can open() it and
// guarantees the legacy data-pool fallback file exists (an empty pool map)
// so dataset-less runs don't die on a missing flows/zeus-runs/data.json.
func (l *Launcher) stageFlow(runID string, flowDoc []byte) (string, error) {
	dir := filepath.Join(l.cfg.K6Dir, "flows", "zeus-runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dataPath := filepath.Join(dir, "data.json")
	if _, err := os.Stat(dataPath); os.IsNotExist(err) {
		if err := os.WriteFile(dataPath, []byte("{}"), 0o644); err != nil {
			return "", err
		}
	}
	flowPath := filepath.Join(dir, runID+".json")
	if err := os.WriteFile(flowPath, flowDoc, 0o644); err != nil {
		return "", err
	}
	return flowPath, nil
}

// transition advances the run's FSM, logging (not failing) a rejected
// transition -- e.g. the run was stopped while we were about to complete it.
// It reports whether the transition happened.
func (l *Launcher) transition(runID, status string) bool {
	if err := l.runs.UpdateStatus(runID, status); err != nil {
		l.cfg.Logger.Warn("launcher: transition rejected", "run_id", runID, "to", status, "error", err)
		return false
	}
	l.publishState(runID, status, "")
	return true
}

// fail finalizes the run as failed with reason; it reports whether the
// failed transition happened (false if the run was already terminal).
func (l *Launcher) fail(runID, reason string) bool {
	l.cfg.Logger.Error("launcher: run failed", "run_id", runID, "reason", reason)
	ok := true
	if err := l.runs.UpdateStatus(runID, StatusFailed); err != nil {
		l.cfg.Logger.Warn("launcher: fail transition rejected", "run_id", runID, "error", err)
		ok = false
	}
	if err := l.runs.UpdateReason(runID, reason); err != nil {
		l.cfg.Logger.Warn("launcher: persist failure reason", "run_id", runID, "error", err)
	}
	l.publishState(runID, StatusFailed, reason)
	return ok
}

func (l *Launcher) publishState(runID, status, detail string) {
	if l.broker == nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"run_id": runID, "status": status, "detail": detail,
	})
	l.broker.Publish(runID, sse.Event{Type: "run.state", Data: string(payload), RunID: runID})
}

// k6Summary is the subset of k6's --summary-export document RunStats needs.
type k6Summary struct {
	Metrics map[string]struct {
		Count  float64            `json:"count"`
		Passes float64            `json:"passes"`
		Fails  float64            `json:"fails"`
		Values map[string]float64 `json:"-"`
		Med    float64            `json:"med"`
		P95    float64            `json:"p(95)"`
		P99    float64            `json:"p(99)"`
	} `json:"metrics"`
}

// parseSummary maps the k6 summary export into a final RunStats snapshot. A
// missing or malformed summary is logged and yields nil -- the run still
// completes, and supervision falls back to the stream totals.
func (l *Launcher) parseSummary(runID, path string) *stats.RunStats {
	raw, err := os.ReadFile(path)
	if err != nil {
		l.cfg.Logger.Warn("launcher: no k6 summary", "run_id", runID, "error", err)
		return nil
	}
	var sum k6Summary
	if err := json.Unmarshal(raw, &sum); err != nil {
		l.cfg.Logger.Warn("launcher: bad k6 summary", "run_id", runID, "error", err)
		return nil
	}
	ms := func(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }

	now := time.Now()
	rs := &stats.RunStats{
		RunID:       runID,
		Status:      StatusCompleted,
		Final:       true,
		Source:      stats.SourceSummary,
		UpdatedAt:   now,
		FinalizedAt: &now,
	}
	if rn, ok := l.runs.Get(runID); ok {
		rs.WorkflowID = rn.WorkflowID
		rs.Duration = time.Since(rn.StartedAt)
	}
	if m, ok := sum.Metrics["iterations"]; ok {
		rs.Iterations = int64(m.Count)
	}
	if m, ok := sum.Metrics["http_reqs"]; ok {
		rs.RequestsSent = int64(m.Count)
	}
	if m, ok := sum.Metrics["http_req_failed"]; ok {
		// Rate metric: "passes" counts samples where the condition (request
		// FAILED) held. OK = total - failed.
		rs.RequestsOK = rs.RequestsSent - int64(m.Passes)
	} else {
		rs.RequestsOK = rs.RequestsSent
	}
	if m, ok := sum.Metrics["dropped_iterations"]; ok {
		rs.RequestsDropped = int64(m.Count)
	}
	if m, ok := sum.Metrics["http_req_duration"]; ok {
		rs.LatencyP50 = ms(m.Med)
		rs.LatencyP95 = ms(m.P95)
		rs.LatencyP99 = ms(m.P99)
	}
	return rs
}

// tailBuffer keeps the last cap bytes written -- enough of k6's output to
// explain a failure without buffering an unbounded log.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newTailBuffer(capBytes int) *tailBuffer {
	return &tailBuffer{cap: capBytes}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
