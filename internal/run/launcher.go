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
	Logger         *slog.Logger
}

// LaunchSpec carries the per-run execution parameters (from the create-run
// request plus the workflow document).
type LaunchSpec struct {
	VUs           int
	DurationS     int
	Persona       string // default "cautious" (a file under K6Dir/personas)
	BaseURL       string // workflow base_url; empty = flow embeds absolute URLs
	DatasetID     string // empty = no dataset (an empty pool map is staged)
	WorkflowLabel string // rides W3C baggage as atropos.workflow
	MetaTraceID   string // rides W3C baggage as meta-trace-id (the id the create-run response echoed)
}

// Launcher supervises one k6 subprocess per run and is the ONLY writer of a
// run's post-create lifecycle: it advances starting -> validating -> running
// -> completing -> completed/failed, parses the k6 summary into the snapshot
// store, and publishes run.state events on the SSE broker. Stop kills the
// subprocess; the DELETE handler owns the 'stopped' status (the launcher
// never overwrites a terminal status -- UpdateStatus enforces the FSM).
type Launcher struct {
	cfg       LauncherConfig
	runs      *Store
	snapshots *stats.SnapshotStore
	broker    *sse.Broker

	mu     sync.Mutex
	cancel map[string]context.CancelFunc
	wg     sync.WaitGroup
}

// NewLauncher wires a Launcher. snapshots and broker may be nil (no stats /
// no events); runs is required.
func NewLauncher(cfg LauncherConfig, runs *Store, snapshots *stats.SnapshotStore, broker *sse.Broker) *Launcher {
	if cfg.K6Bin == "" {
		cfg.K6Bin = "k6"
	}
	if cfg.K6Dir == "" {
		cfg.K6Dir = "./k6"
	}
	if cfg.MaxRunDuration <= 0 {
		cfg.MaxRunDuration = 30 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Launcher{cfg: cfg, runs: runs, snapshots: snapshots, broker: broker, cancel: map[string]context.CancelFunc{}}
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

	summaryPath := filepath.Join(os.TempDir(), "zeus-run-"+rn.ID+"-summary.json")

	args := []string{
		"run",
		"--quiet",
		"--summary-export", summaryPath,
		// p(99) is not in k6's default trend stats; RunStats wants it.
		"--summary-trend-stats", "avg,min,med,max,p(90),p(95),p(99)",
		"-e", "FLOW=zeus-runs/" + rn.ID,
		"-e", "ZEUS_RUN_ID=" + rn.ID,
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
		_ = os.Remove(flowPath)
		l.fail(rn.ID, "k6 start: "+err.Error())
		return err
	}

	l.mu.Lock()
	l.cancel[rn.ID] = cancel
	l.mu.Unlock()

	l.transition(rn.ID, StatusRunning)

	l.wg.Add(1)
	go l.supervise(ctx, cmd, rn.ID, flowPath, summaryPath, tail)
	return nil
}

// Stop kills the run's k6 subprocess, if one is live. The caller (the
// DELETE handler) owns the 'stopped' status transition; Stop only signals.
// Unknown ids are a no-op.
func (l *Launcher) Stop(runID string) {
	l.mu.Lock()
	cancel := l.cancel[runID]
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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

// supervise waits for the k6 process, finalizes the run's status, parses
// the summary into the snapshot store, and closes the run's SSE stream.
func (l *Launcher) supervise(ctx context.Context, cmd *exec.Cmd, runID, flowPath, summaryPath string, tail *tailBuffer) {
	defer l.wg.Done()
	err := cmd.Wait()

	l.mu.Lock()
	cancel := l.cancel[runID]
	delete(l.cancel, runID)
	l.mu.Unlock()

	defer func() {
		if cancel != nil {
			cancel() // release the deadline timer (runs after the ctx.Err() branching)
		}
		_ = os.Remove(flowPath)
		_ = os.Remove(summaryPath)
		if l.broker != nil {
			l.broker.CloseRun(runID)
		}
	}()

	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The watchdog killed a wedged or overrunning k6 -- a
			// launcher-owned failure, not an operator stop.
			l.fail(runID, "run deadline exceeded")
			return
		}
		// Killed via Stop/Close: the stop path owns the terminal status
		// ('stopped'); just report what happened.
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
			l.fail(runID, reason)
			return
		}
		if uerr := l.runs.UpdateReason(runID, "thresholds breached"); uerr != nil {
			l.cfg.Logger.Warn("launcher: persist breach reason", "run_id", runID, "error", uerr)
		}
	}

	l.transition(runID, StatusCompleting)
	if rs := l.parseSummary(runID, summaryPath); rs != nil && l.snapshots != nil {
		l.snapshots.Save(rs)
	}
	l.transition(runID, StatusCompleted)
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
func (l *Launcher) transition(runID, status string) {
	if err := l.runs.UpdateStatus(runID, status); err != nil {
		l.cfg.Logger.Warn("launcher: transition rejected", "run_id", runID, "to", status, "error", err)
		return
	}
	l.publishState(runID, status, "")
}

func (l *Launcher) fail(runID, reason string) {
	l.cfg.Logger.Error("launcher: run failed", "run_id", runID, "reason", reason)
	if err := l.runs.UpdateStatus(runID, StatusFailed); err != nil {
		l.cfg.Logger.Warn("launcher: fail transition rejected", "run_id", runID, "error", err)
	}
	if err := l.runs.UpdateReason(runID, reason); err != nil {
		l.cfg.Logger.Warn("launcher: persist failure reason", "run_id", runID, "error", err)
	}
	l.publishState(runID, StatusFailed, reason)
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

// parseSummary maps the k6 summary export into a RunStats snapshot. A
// missing or malformed summary is logged and skipped -- the run still
// completes; only its stats endpoint stays empty.
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

	rs := &stats.RunStats{RunID: runID, Status: StatusCompleted, FinalizedAt: time.Now()}
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
