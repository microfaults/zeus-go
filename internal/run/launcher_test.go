package run

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atropos-go/loadgen/internal/sse"
	"atropos-go/loadgen/internal/stats"
)

// stubK6 writes a shell script that impersonates k6: it scans its args for
// --summary-export <path>, writes `summary` there, and exits with `code`.
// This lets the launcher's real supervision path (exec, wait, parse, FSM)
// run without a k6 install.
func stubK6(t *testing.T, summary string, code int) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "k6stub.sh")
	script := `#!/bin/sh
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--summary-export" ]; then out="$2"; shift; fi
  shift
done
if [ -n "$out" ]; then printf '%s' '` + summary + `' > "$out"; fi
exit ` + strconv.Itoa(code) + `
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// stubK6ArgsDump writes a k6 stand-in that records its argv (one arg per
// line) to argsPath and exits cleanly, for asserting the constructed
// command line.
func stubK6ArgsDump(t *testing.T, argsPath string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "k6stub.sh")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsPath + `"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

const goodSummary = `{"metrics":{"iterations":{"count":120},"http_reqs":{"count":120},"http_req_failed":{"passes":0},"http_req_duration":{"med":5,"p(95)":12,"p(99)":30}}}`

func newTestLauncher(t *testing.T, k6bin string) (*Launcher, *Store, *stats.SnapshotStore, *sse.Broker) {
	t.Helper()
	runs := NewStore()
	snaps := stats.NewSnapshotStore()
	broker := sse.NewBroker()
	// K6Dir must contain a runner.js for stageFlow's sibling writes; the stub
	// ignores it, but cmd.Dir must exist.
	k6dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(k6dir, "runner.js"), []byte("//stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := NewLauncher(LauncherConfig{K6Bin: k6bin, K6Dir: k6dir}, runs, snaps, broker)
	return l, runs, snaps, broker
}

func seedRun(t *testing.T, runs *Store, id string) *Run {
	t.Helper()
	rn := &Run{ID: id, WorkflowID: "wf-1", Status: StatusStarting, StartedAt: time.Now()}
	if err := runs.Create(rn); err != nil {
		t.Fatal(err)
	}
	return rn
}

func waitStatus(t *testing.T, runs *Store, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rn, ok := runs.Get(id); ok && rn.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	rn, _ := runs.Get(id)
	t.Fatalf("run %s did not reach %q; stuck at %q", id, want, rn.Status)
}

// TestLauncher_HappyPath: a clean k6 exit advances the FSM to completed and
// the summary lands in the snapshot store.
func TestLauncher_HappyPath(t *testing.T) {
	l, runs, snaps, _ := newTestLauncher(t, stubK6(t, goodSummary, 0))
	defer l.Close()
	rn := seedRun(t, runs, "run-ok")

	if err := l.Launch(rn, []byte(`{"version":"2","root":{}}`), LaunchSpec{VUs: 5, DurationS: 1}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitStatus(t, runs, "run-ok", StatusCompleted)

	rs, ok := snaps.Get("run-ok")
	if !ok {
		t.Fatal("no snapshot saved for completed run")
	}
	if rs.Iterations != 120 || rs.RequestsSent != 120 || rs.RequestsOK != 120 {
		t.Fatalf("summary parse wrong: %+v", rs)
	}
	if rs.LatencyP95 != 12*time.Millisecond {
		t.Fatalf("p95 = %v, want 12ms", rs.LatencyP95)
	}
}

// TestLauncher_MetaTraceIDEnv: the meta_trace_id minted at run creation must
// reach k6 as ZEUS_META_TRACE_ID — otherwise the engine mints its own id and
// every request's baggage disagrees with the id zeus reported (Z1).
func TestLauncher_MetaTraceIDEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		traceID string
	}{
		{"injected when set", "trace-z1"},
		{"omitted when empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsPath := filepath.Join(t.TempDir(), "args.txt")
			l, runs, _, _ := newTestLauncher(t, stubK6ArgsDump(t, argsPath))
			defer l.Close()
			rn := seedRun(t, runs, "run-trace")

			if err := l.Launch(rn, []byte(`{}`), LaunchSpec{MetaTraceID: tc.traceID}); err != nil {
				t.Fatalf("launch: %v", err)
			}
			waitStatus(t, runs, "run-trace", StatusCompleted)

			raw, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("read args dump: %v", err)
			}
			if tc.traceID != "" {
				if !strings.Contains(string(raw), "-e\nZEUS_META_TRACE_ID="+tc.traceID+"\n") {
					t.Fatalf("k6 args missing ZEUS_META_TRACE_ID env pair; args:\n%s", raw)
				}
			} else if strings.Contains(string(raw), "ZEUS_META_TRACE_ID") {
				t.Fatalf("k6 args must not carry an empty ZEUS_META_TRACE_ID; args:\n%s", raw)
			}
		})
	}
}

// TestLauncher_FailingK6: a non-zero k6 exit finalizes the run as failed with
// the tail captured in the reason and no stats snapshot.
func TestLauncher_FailingK6(t *testing.T) {
	l, runs, snaps, _ := newTestLauncher(t, stubK6(t, "", 1))
	defer l.Close()
	rn := seedRun(t, runs, "run-fail")

	if err := l.Launch(rn, []byte(`{}`), LaunchSpec{DurationS: 1}); err != nil {
		t.Fatalf("launch (start should still succeed): %v", err)
	}
	waitStatus(t, runs, "run-fail", StatusFailed)
	if got, _ := runs.Get("run-fail"); got.Reason == "" {
		t.Fatal("failed run should carry a reason")
	}
	if _, ok := snaps.Get("run-fail"); ok {
		t.Fatal("failed run must not save a stats snapshot")
	}
}

// TestLauncher_ThresholdBreachCompletes: k6 exit 99 means thresholds were
// crossed, not that execution failed -- the load ran to completion and the
// summary export is intact. A degraded mesh is the experiment working as
// designed, so the run completes with its stats preserved and the breach
// recorded as the reason (Z2).
func TestLauncher_ThresholdBreachCompletes(t *testing.T) {
	l, runs, snaps, _ := newTestLauncher(t, stubK6(t, goodSummary, 99))
	defer l.Close()
	rn := seedRun(t, runs, "run-breach")

	if err := l.Launch(rn, []byte(`{}`), LaunchSpec{DurationS: 1}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitStatus(t, runs, "run-breach", StatusCompleted)

	rs, ok := snaps.Get("run-breach")
	if !ok {
		t.Fatal("threshold breach destroyed the stats snapshot")
	}
	if rs.Iterations != 120 {
		t.Fatalf("summary parse wrong: %+v", rs)
	}
	if got, _ := runs.Get("run-breach"); got.Reason != "thresholds breached" {
		t.Fatalf("reason = %q, want %q", got.Reason, "thresholds breached")
	}
}

// TestLauncher_StageAndCleanup: the staged flow file is removed after the run
// finishes (no orphaned per-run documents accumulate under flows/).
func TestLauncher_StageAndCleanup(t *testing.T) {
	l, runs, _, _ := newTestLauncher(t, stubK6(t, goodSummary, 0))
	defer l.Close()
	rn := seedRun(t, runs, "run-clean")
	if err := l.Launch(rn, []byte(`{"version":"2","root":{}}`), LaunchSpec{DurationS: 1}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitStatus(t, runs, "run-clean", StatusCompleted)

	staged := filepath.Join(l.cfg.K6Dir, "flows", "zeus-runs", "run-clean.json")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged flow doc was not cleaned up: %v", err)
	}
}
