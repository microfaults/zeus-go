package run

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"atropos-go/loadgen/internal/k6fake"
	"atropos-go/loadgen/internal/stats"
)

// fakeK6 writes a k6fake executable for cfg.
func fakeK6(t *testing.T, cfg k6fake.Config) string {
	t.Helper()
	bin, err := k6fake.Write(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// runSeries returns, per metric family carrying run_id=runID, the label
// names of its series and the summed counter/gauge value or histogram
// sample count.
func runSeries(t *testing.T, m *stats.Metrics, runID string) (labels map[string][]string, values map[string]float64) {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels, values = map[string][]string{}, map[string]float64{}
	for _, mf := range mfs {
		for _, mt := range mf.GetMetric() {
			if !hasLabel(mt, "run_id", runID) {
				continue
			}
			var names []string
			for _, lp := range mt.GetLabel() {
				names = append(names, lp.GetName())
			}
			sort.Strings(names)
			labels[mf.GetName()] = names
			switch {
			case mt.Counter != nil:
				values[mf.GetName()] += mt.GetCounter().GetValue()
			case mt.Gauge != nil:
				values[mf.GetName()] += mt.GetGauge().GetValue()
			case mt.Histogram != nil:
				values[mf.GetName()] += float64(mt.GetHistogram().GetSampleCount())
			}
		}
	}
	return labels, values
}

func hasLabel(mt *dto.Metric, name, value string) bool {
	for _, lp := range mt.GetLabel() {
		if lp.GetName() == name && lp.GetValue() == value {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestLauncher_LiveMetricsWhileRunning: while k6 runs, every per-run metric
// family is fed from k6's sample stream and advances, with exactly the label
// set the registry declares; /stats-backing snapshots are interim; and the
// final snapshot still comes from the summary export.
func TestLauncher_LiveMetricsWhileRunning(t *testing.T) {
	cfg := k6fake.Config{
		Ticks: 20, TickInterval: 100 * time.Millisecond,
		ReqsPerTick: 4, DroppedPerTick: 1, VUs: 7,
		Summary: goodSummary,
	}
	l, runs, snaps, broker := newTestLauncherCfg(t, LauncherConfig{
		K6Bin:            fakeK6(t, cfg),
		SnapshotInterval: 50 * time.Millisecond,
	})
	defer l.Close()
	m := l.metrics
	rn := seedRun(t, runs, "run-live")
	events, unsub := broker.Subscribe("run-live")
	defer unsub()

	if err := l.Launch(rn, []byte(`{}`), LaunchSpec{DurationS: 10}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	running := func() bool { r, _ := runs.Get("run-live"); return r.Status == StatusRunning }
	sent := func() float64 { _, v := runSeries(t, m, "run-live"); return v["zeus_requests_sent_total"] }

	waitFor(t, "first live samples", func() bool { return sent() > 0 })
	first := sent()
	if !running() {
		t.Fatal("run finished before live metrics could be observed; slow the fake down")
	}
	waitFor(t, "live samples to advance", func() bool { return sent() > first })
	if !running() {
		t.Fatal("metrics only advanced after the run ended")
	}

	labels, values := runSeries(t, m, "run-live")
	wantLabels := map[string][]string{
		"zeus_requests_sent_total":        {"run_id", "step_id", "workflow_id"},
		"zeus_requests_ok_total":          {"run_id", "step_id", "workflow_id"},
		"zeus_requests_dropped_total":     {"reason", "run_id", "step_id", "workflow_id"},
		"zeus_iterations_total":           {"run_id", "workflow_id"},
		"zeus_request_duration_seconds":   {"run_id", "step_id", "workflow_id"},
		"zeus_iteration_duration_seconds": {"run_id", "workflow_id"},
		"zeus_active_vus":                 {"run_id"},
	}
	for name, want := range wantLabels {
		if fmt.Sprint(labels[name]) != fmt.Sprint(want) {
			t.Errorf("%s labels = %v, want %v (written while running?)", name, labels[name], want)
		}
	}
	for name := range labels {
		if _, ok := wantLabels[name]; !ok {
			t.Errorf("unexpected per-run family %s", name)
		}
	}
	if values["zeus_active_vus"] != 7 {
		t.Errorf("active vus = %v while running, want 7", values["zeus_active_vus"])
	}

	// Interim snapshot while running.
	rs, ok := snaps.Get("run-live")
	if !ok || rs.Final || rs.Source != stats.SourceStream || rs.Status != StatusRunning {
		t.Fatalf("interim snapshot = %+v, want non-final stream snapshot of a running run", rs)
	}
	waitFor(t, "interim snapshot with counts", func() bool {
		rs, _ := snaps.Get("run-live")
		return rs.RequestsSent > 0 && rs.LatencyP50 > 0 && rs.Iterations > 0
	})

	waitStatus(t, runs, "run-live", StatusCompleted)
	waitFinal(t, snaps, "run-live")

	// Stream totals land in Prometheus in full...
	_, values = runSeries(t, m, "run-live")
	if values["zeus_requests_sent_total"] != float64(cfg.Sent()) ||
		values["zeus_requests_ok_total"] != float64(cfg.OK()) ||
		values["zeus_iterations_total"] != float64(cfg.Ticks) ||
		values["zeus_requests_dropped_total"] != float64(cfg.Ticks*cfg.DroppedPerTick) ||
		values["zeus_request_duration_seconds"] != float64(cfg.Sent()) {
		t.Errorf("final stream totals in Prometheus = %v", values)
	}
	if values["zeus_active_vus"] != 0 {
		t.Errorf("active vus = %v after the run, want 0", values["zeus_active_vus"])
	}
	// ...but the final snapshot is the summary export's (120 != streamed 80).
	rs, _ = snaps.Get("run-live")
	if !rs.Final || rs.Source != stats.SourceSummary || rs.RequestsSent != 120 || rs.Iterations != 120 {
		t.Fatalf("final snapshot = %+v, want summary-export numbers", rs)
	}

	// The event stream carried interim stats.snapshot events and a final one.
	l.Close()
	var interim, final int
	for ev := range events {
		if ev.Type != "stats.snapshot" {
			continue
		}
		var got stats.RunStats
		if err := json.Unmarshal([]byte(ev.Data), &got); err != nil {
			t.Fatalf("bad stats.snapshot payload: %v", err)
		}
		if got.Final {
			final++
		} else {
			interim++
		}
	}
	if interim < 5 || final != 1 {
		t.Fatalf("stats.snapshot events: %d interim, %d final; want >= 5 interim and 1 final", interim, final)
	}
}

// TestLauncher_StopKeepsStreamStats: a run killed by DELETE keeps the stats
// gathered up to the stop as its final snapshot, with its stopped status.
func TestLauncher_StopKeepsStreamStats(t *testing.T) {
	cfg := k6fake.Config{
		Ticks: 3, TickInterval: 10 * time.Millisecond,
		ReqsPerTick: 4, VUs: 3, Hold: true,
	}
	l, runs, snaps, _ := newTestLauncherCfg(t, LauncherConfig{
		K6Bin:            fakeK6(t, cfg),
		SnapshotInterval: 20 * time.Millisecond,
	})
	defer l.Close()
	rn := seedRun(t, runs, "run-stopstats")
	if err := l.Launch(rn, []byte(`{}`), LaunchSpec{}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, "streamed samples in the interim snapshot", func() bool {
		rs, ok := snaps.Get("run-stopstats")
		return ok && rs.RequestsSent == int64(cfg.Sent())
	})

	// Mimic handleStopRun: status first, then kill.
	if err := runs.UpdateStatus("run-stopstats", StatusStopped); err != nil {
		t.Fatal(err)
	}
	if !l.Stop("run-stopstats") {
		t.Fatal("Stop reported no live process")
	}
	l.Close()

	rs, ok := snaps.Get("run-stopstats")
	if !ok || !rs.Final || rs.Status != StatusStopped || rs.Source != stats.SourceStream {
		t.Fatalf("stopped snapshot = %+v, want final stream snapshot with status stopped", rs)
	}
	if rs.RequestsSent != int64(cfg.Sent()) || rs.RequestsOK != int64(cfg.OK()) || rs.Iterations != int64(cfg.Ticks) {
		t.Fatalf("stopped snapshot lost stats: %+v", rs)
	}
	if rs.LatencyP99 < 39*time.Millisecond || rs.LatencyP99 > 41*time.Millisecond {
		t.Fatalf("p99 = %v, want ~40ms", rs.LatencyP99)
	}
	if got, _ := runs.Get("run-stopstats"); got.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
}

// TestStreamSamples_Parse: Points of the metrics zeus uses are ingested;
// declarations, ignored metrics, console noise, malformed and oversized
// lines are skipped without stopping the read.
func TestStreamSamples_Parse(t *testing.T) {
	m := stats.NewMetrics()
	live := stats.NewLiveRun(m, "r1", "wf", time.Now())
	huge := `{"metric":"http_reqs","type":"Point","data":{"value":1,"tags":{"name":"` + strings.Repeat("x", maxSampleLine*2) + `"}}}`
	input := strings.Join([]string{
		`{"type":"Metric","data":{"name":"http_reqs","type":"counter"},"metric":"http_reqs"}`,
		`{"metric":"http_reqs","type":"Point","data":{"time":"t","value":1,"tags":{"name":"home"}}}`,
		`{"metric":"http_req_failed","type":"Point","data":{"value":0,"tags":{"name":"home"}}}`,
		`{"metric":"http_req_duration","type":"Point","data":{"value":12.5,"tags":{"name":"home"}}}`,
		`{"metric":"http_req_waiting","type":"Point","data":{"value":12,"tags":{"name":"home"}}}`,
		huge,
		`not json at all`,
		``,
		`{"type":"Point","metric":"iterations","data":{"value":1,"tags":{}}}`, // field order k6 does not use
		`{"metric":"vus","type":"Point","data":{"value":4,"tags":{}}}`,
	}, "\n") // no trailing newline: the last line must still count

	bad, err := streamSamples(strings.NewReader(input), live)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 2 {
		t.Errorf("bad lines = %d, want 2 (oversized + malformed)", bad)
	}
	rs := live.Snapshot(StatusRunning, false)
	if rs.RequestsSent != 1 || rs.RequestsOK != 1 || rs.Iterations != 1 || rs.ActiveVUs != 4 {
		t.Fatalf("snapshot = %+v", rs)
	}
	labels, _ := runSeries(t, m, "r1")
	if fmt.Sprint(labels["zeus_requests_sent_total"]) != "[run_id step_id workflow_id]" {
		t.Fatalf("labels = %v", labels)
	}
}

// sampleGenerator produces n realistic k6 sample lines on the fly, so the
// test input itself is never resident.
type sampleGenerator struct {
	n, i int
	buf  []byte
}

func (g *sampleGenerator) Read(p []byte) (int, error) {
	for len(g.buf) < len(p) && g.i < g.n {
		metric := [...]string{"http_reqs", "http_req_duration", "http_req_failed", "http_req_blocked", "iterations"}[g.i%5]
		g.buf = fmt.Appendf(g.buf, `{"metric":"%s","type":"Point","data":{"time":"2026-11-16T12:00:00Z","value":%d,"tags":{"expected_response":"true","group":"","method":"GET","name":"step-%d","proto":"HTTP/1.1","scenario":"workflow","status":"200","url":"http://target.local/p/%d"}}}`+"\n",
			metric, 1, g.i%7, g.i)
		g.i++
	}
	if len(g.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, g.buf)
	g.buf = g.buf[n:]
	return n, nil
}

// TestStreamSamples_BoundedMemory: aggregation keeps no per-sample state,
// so heap stays flat however many samples a long run streams.
func TestStreamSamples_BoundedMemory(t *testing.T) {
	const lines = 300_000 // ~85 MB of k6 output
	live := stats.NewLiveRun(stats.NewMetrics(), "r-mem", "wf", time.Now())

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := streamSamples(&sampleGenerator{n: lines}, live); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	if rs := live.Snapshot(StatusRunning, false); rs.RequestsSent != int64(lines/5) {
		t.Fatalf("requests sent = %d, want %d", rs.RequestsSent, lines/5)
	}
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > 8<<20 {
		t.Fatalf("heap grew %d bytes over %d samples; aggregation must not retain samples", growth, lines)
	}
	runtime.KeepAlive(live)
}
