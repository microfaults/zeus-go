package run

import (
	"os"
	"path/filepath"
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
exit ` + itoa(code) + `
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
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

// TestLauncher_FailingK6: a non-zero k6 exit finalizes the run as failed with
// the tail captured in the reason.
func TestLauncher_FailingK6(t *testing.T) {
	l, runs, _, _ := newTestLauncher(t, stubK6(t, "", 1))
	defer l.Close()
	rn := seedRun(t, runs, "run-fail")

	if err := l.Launch(rn, []byte(`{}`), LaunchSpec{DurationS: 1}); err != nil {
		t.Fatalf("launch (start should still succeed): %v", err)
	}
	waitStatus(t, runs, "run-fail", StatusFailed)
	if got, _ := runs.Get("run-fail"); got.Reason == "" {
		t.Fatal("failed run should carry a reason")
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
