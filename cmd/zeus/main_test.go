package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atropos-go/loadgen/internal/api"
	"atropos-go/loadgen/internal/k6fake"
)

// These tests drive zeus as wired by newApp, with a fake k6 selected via
// ZEUS_K6_BIN, over real HTTP with deliberately short server timeouts.

const summaryExport = `{"metrics":{"iterations":{"count":120},"http_reqs":{"count":120},"http_req_failed":{"passes":6},"http_req_duration":{"med":5,"p(95)":12,"p(99)":30}}}`

// shortTimeouts make the 30s production write timeout's effect observable in
// a short test: a run longer than Write would be cut without the SSE fix.
var shortTimeouts = api.HTTPTimeouts{Read: 300 * time.Millisecond, Write: 500 * time.Millisecond, Idle: 2 * time.Second}

func startZeus(t *testing.T, fake k6fake.Config) (string, *app) {
	t.Helper()
	bin, err := k6fake.Write(t.TempDir(), fake)
	if err != nil {
		t.Fatal(err)
	}
	k6dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(k6dir, "runner.js"), []byte("// fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEUS_K6_BIN", bin)
	t.Setenv("ZEUS_K6_DIR", k6dir)

	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), appOptions{
		HTTPTimeouts:     shortTimeouts,
		SSEHeartbeat:     100 * time.Millisecond,
		SnapshotInterval: 100 * time.Millisecond,
	})
	srv := httptest.NewUnstartedServer(a.server.Handler)
	srv.Config = a.server
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		a.launcher.Close()
	})
	return srv.URL, a
}

func doJSON(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

// startRun registers a workflow and starts a run of it; returns the run id.
func startRun(t *testing.T, base string) string {
	t.Helper()
	var wf struct{ ID string }
	if code := doJSON(t, "POST", base+"/api/v1/workflows", `{"workflow":{"name":"browse","version":"2",
		"targets":["frontend"],"estimated_rps_per_vu":2,
		"root":{"type":"request","id":"home","path":"/","method":"GET"}}}`, &wf); code != http.StatusCreated {
		t.Fatalf("create workflow: %d", code)
	}
	var created struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if code := doJSON(t, "POST", base+"/api/v1/workflows/"+wf.ID+"/runs",
		`{"experiment_id":"exp-1","vus":5,"duration_s":30,"labels":{"phase":"baseline"}}`, &created); code != http.StatusAccepted {
		t.Fatalf("create run: %d", code)
	}
	if created.RunID == "" || created.Status != "starting" {
		t.Fatalf("create run response = %+v", created)
	}
	return created.RunID
}

type runStats struct {
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Final        bool   `json:"final"`
	Source       string `json:"source"`
	RequestsSent int64  `json:"requests_sent"`
	RequestsOK   int64  `json:"requests_ok"`
	Iterations   int64  `json:"iterations"`
	LatencyP50   int64  `json:"latency_p50"`
	LatencyP95   int64  `json:"latency_p95"`
}

func getStats(t *testing.T, base, runID string) runStats {
	t.Helper()
	var rs runStats
	if code := doJSON(t, "GET", base+"/api/v1/runs/"+runID+"/stats", "", &rs); code != http.StatusOK {
		t.Fatalf("stats: %d", code)
	}
	return rs
}

func runStatus(t *testing.T, base, runID string) string {
	t.Helper()
	var rn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if code := doJSON(t, "GET", base+"/api/v1/runs/"+runID, "", &rn); code != http.StatusOK || rn.ID != runID {
		t.Fatalf("get run: %d %+v", code, rn)
	}
	return rn.Status
}

// scrape sums the samples of a family for runID on GET /api/v1/metrics.
func scrape(t *testing.T, base, family, runID string) float64 {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sum float64
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, family+"{") || !strings.Contains(line, `run_id="`+runID+`"`) {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		if err != nil {
			t.Fatalf("bad sample line %q", line)
		}
		sum += v
	}
	return sum
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type sseItem struct {
	at        time.Time
	heartbeat bool
	event     string
	data      string
}

// tail reads an SSE stream in the background until the server closes it.
type tail struct {
	mu     sync.Mutex
	items  []sseItem
	opened time.Time
	closed time.Time
	err    error
	done   chan struct{}
}

func openEvents(t *testing.T, base, runID string) *tail {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/runs/" + runID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	tl := &tail{opened: time.Now(), done: make(chan struct{})}
	go func() {
		defer close(tl.done)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		var ev sseItem
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				tl.mu.Lock()
				tl.closed = time.Now()
				if err != io.EOF {
					tl.err = err
				}
				tl.mu.Unlock()
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, ":"):
				tl.add(sseItem{at: time.Now(), heartbeat: true})
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.event != "":
				ev.at = time.Now()
				tl.add(ev)
				ev = sseItem{}
			}
		}
	}()
	return tl
}

func (tl *tail) add(it sseItem) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.items = append(tl.items, it)
}

func (tl *tail) wait(t *testing.T) {
	t.Helper()
	select {
	case <-tl.done:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream never closed")
	}
}

// TestE2E_LiveRunObservability covers a full run end to end: metrics
// advance on /api/v1/metrics while the run is running, /stats serves an
// interim snapshot and then the summary export's numbers, and the event
// stream stays open for the whole run (well past the server's write and
// read timeouts) with heartbeats and periodic stats.snapshot events.
func TestE2E_LiveRunObservability(t *testing.T) {
	fake := k6fake.Config{
		Ticks: 25, TickInterval: 100 * time.Millisecond, // ~2.5s, 5x the write timeout
		ReqsPerTick: 4, DroppedPerTick: 1, VUs: 6,
		Summary: summaryExport,
	}
	base, _ := startZeus(t, fake)
	runID := startRun(t, base)
	events := openEvents(t, base, runID)

	// Prometheus advances while the run is running.
	waitFor(t, "requests on /metrics", func() bool { return scrape(t, base, "zeus_requests_sent_total", runID) > 0 })
	first := scrape(t, base, "zeus_requests_sent_total", runID)
	waitFor(t, "requests on /metrics to advance", func() bool {
		return scrape(t, base, "zeus_requests_sent_total", runID) > first
	})
	if s := runStatus(t, base, runID); s != "running" {
		t.Fatalf("metrics only advanced once the run was %s", s)
	}
	for _, fam := range []string{"zeus_requests_ok_total", "zeus_iterations_total", "zeus_requests_dropped_total",
		"zeus_request_duration_seconds_count", "zeus_iteration_duration_seconds_count"} {
		if scrape(t, base, fam, runID) <= 0 {
			t.Errorf("%s not written while running", fam)
		}
	}
	if v := scrape(t, base, "zeus_active_vus", runID); v != 6 {
		t.Errorf("zeus_active_vus = %v while running, want 6", v)
	}

	// Interim /stats while running.
	rs := getStats(t, base, runID)
	if rs.Final || rs.Source != "k6_stream" || rs.Status != "running" || rs.RequestsSent == 0 || rs.LatencyP50 == 0 {
		t.Fatalf("interim stats = %+v", rs)
	}

	// Final /stats come from the summary export.
	waitFor(t, "final stats", func() bool { return getStats(t, base, runID).Final })
	rs = getStats(t, base, runID)
	if rs.Status != "completed" || rs.Source != "k6_summary" || rs.RequestsSent != 120 || rs.RequestsOK != 114 ||
		rs.Iterations != 120 || rs.LatencyP95 != int64(12*time.Millisecond) {
		t.Fatalf("final stats = %+v, want the summary export's numbers", rs)
	}
	if v := scrape(t, base, "zeus_active_vus", runID); v != 0 {
		t.Errorf("zeus_active_vus = %v after the run, want 0", v)
	}
	if v := scrape(t, base, "zeus_requests_sent_total", runID); v != float64(fake.Sent()) {
		t.Errorf("zeus_requests_sent_total = %v after the run, want all %d streamed requests", v, fake.Sent())
	}

	// The event stream lasted the whole run and was closed by zeus.
	events.wait(t)
	events.mu.Lock()
	defer events.mu.Unlock()
	if events.err != nil {
		t.Fatalf("stream broke: %v", events.err)
	}
	if lifetime := events.closed.Sub(events.opened); lifetime < 4*shortTimeouts.Write {
		t.Fatalf("stream lived %v; expected it to outlast the %v write timeout for the ~2.5s run", lifetime, shortTimeouts.Write)
	}
	var heartbeats, interim, final int
	var lastSnap time.Time
	var maxGap time.Duration
	var lastState string
	for _, it := range events.items {
		switch {
		case it.heartbeat:
			heartbeats++
		case it.event == "stats.snapshot":
			var s runStats
			if err := json.Unmarshal([]byte(it.data), &s); err != nil {
				t.Fatalf("stats.snapshot data: %v", err)
			}
			if s.Final {
				final++
			} else {
				interim++
				if !lastSnap.IsZero() && it.at.Sub(lastSnap) > maxGap {
					maxGap = it.at.Sub(lastSnap)
				}
				lastSnap = it.at
			}
		case it.event == "run.state":
			var st struct{ Status string }
			_ = json.Unmarshal([]byte(it.data), &st)
			lastState = st.Status
		default:
			t.Errorf("unexpected event type %q", it.event)
		}
	}
	if heartbeats < 5 || interim < 10 || final != 1 || lastState != "completed" {
		t.Fatalf("stream: %d heartbeats, %d interim + %d final snapshots, last state %q",
			heartbeats, interim, final, lastState)
	}
	if maxGap > time.Second {
		t.Fatalf("gap between interim snapshots %v; configured interval is 100ms", maxGap)
	}

	// A subscriber arriving after the end gets the state and final stats,
	// then the stream closes instead of hanging.
	late := openEvents(t, base, runID)
	late.wait(t)
	late.mu.Lock()
	defer late.mu.Unlock()
	if len(late.items) != 2 || late.items[0].event != "run.state" || late.items[1].event != "stats.snapshot" {
		t.Fatalf("late subscriber got %+v", late.items)
	}
}

// TestE2E_StopKeepsStats: DELETE keeps the stats gathered up to the stop,
// with the run's stopped status, and the event stream delivers them before
// closing.
func TestE2E_StopKeepsStats(t *testing.T) {
	fake := k6fake.Config{
		Ticks: 3, TickInterval: 20 * time.Millisecond,
		ReqsPerTick: 4, VUs: 2, Hold: true, // keeps "running" until killed
	}
	base, _ := startZeus(t, fake)
	runID := startRun(t, base)
	events := openEvents(t, base, runID)

	waitFor(t, "all streamed requests in interim stats", func() bool {
		return getStats(t, base, runID).RequestsSent == int64(fake.Sent())
	})
	if code := doJSON(t, "DELETE", base+"/api/v1/runs/"+runID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := doJSON(t, "DELETE", base+"/api/v1/runs/"+runID, "", nil); code != http.StatusConflict {
		t.Fatalf("second delete: %d, want 409", code)
	}

	waitFor(t, "final stats", func() bool { return getStats(t, base, runID).Final })
	rs := getStats(t, base, runID)
	if rs.Status != "stopped" || rs.Source != "k6_stream" || rs.RequestsSent != int64(fake.Sent()) ||
		rs.RequestsOK != int64(fake.OK()) || rs.Iterations != int64(fake.Ticks) {
		t.Fatalf("stopped run stats = %+v", rs)
	}
	if s := runStatus(t, base, runID); s != "stopped" {
		t.Fatalf("run status = %q", s)
	}
	// Series survive the stop (pruned only on eviction).
	if v := scrape(t, base, "zeus_requests_sent_total", runID); v != float64(fake.Sent()) {
		t.Fatalf("zeus_requests_sent_total after stop = %v", v)
	}

	events.wait(t)
	events.mu.Lock()
	defer events.mu.Unlock()
	var sawFinal bool
	for _, it := range events.items {
		if it.event == "stats.snapshot" && strings.Contains(it.data, `"final":true`) && strings.Contains(it.data, `"status":"stopped"`) {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Fatal("event stream closed without the stopped run's final stats.snapshot")
	}
}

// TestNewApp_DefaultTimeouts: production wiring keeps bounded timeouts for
// every endpoint (only the event stream lifts them per request).
func TestNewApp_DefaultTimeouts(t *testing.T) {
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), appOptions{})
	defer a.launcher.Close()
	if a.server.ReadTimeout != 10*time.Second || a.server.WriteTimeout != 30*time.Second || a.server.IdleTimeout != 60*time.Second {
		t.Fatalf("timeouts = %v/%v/%v", a.server.ReadTimeout, a.server.WriteTimeout, a.server.IdleTimeout)
	}
}

// TestNewApp_EvictionPrunesRunSeries: per-run series outlive the run (a
// scraper still sees the final values) and are removed exactly when the run
// record is evicted.
func TestNewApp_EvictionPrunesRunSeries(t *testing.T) {
	base, a := startZeus(t, k6fake.Config{Ticks: 2, TickInterval: 10 * time.Millisecond, ReqsPerTick: 2, VUs: 1, Summary: summaryExport})
	runID := startRun(t, base)
	waitFor(t, "final stats", func() bool { return getStats(t, base, runID).Final })
	if scrape(t, base, "zeus_requests_sent_total", runID) == 0 {
		t.Fatal("completed run's series gone before eviction")
	}

	if err := a.runs.Delete(runID); err != nil {
		t.Fatal(err)
	}
	for _, fam := range []string{"zeus_requests_sent_total", "zeus_requests_ok_total", "zeus_iterations_total",
		"zeus_request_duration_seconds_count", "zeus_iteration_duration_seconds_count", "zeus_active_vus"} {
		if v := scrape(t, base, fam, runID); v != 0 {
			t.Errorf("%s still exported after eviction: %v", fam, v)
		}
	}
	resp, err := http.Get(base + "/api/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), runID) {
		t.Fatalf("evicted run id still on /metrics")
	}
}
