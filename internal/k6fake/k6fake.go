// Package k6fake writes a stand-in k6 executable for tests: a POSIX shell
// script that, invoked like the launcher invokes k6, streams realistic k6
// JSON output lines (the `--out json=<path>` format of k6 0.55) over time,
// then writes a summary export and exits. Point ZEUS_K6_BIN (or
// run.LauncherConfig.K6Bin) at the returned path.
package k6fake

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config shapes the fake run. Each tick emits, in k6's line format: one
// vus gauge sample, ReqsPerTick requests (http_reqs, http_req_duration,
// http_req_failed plus metrics zeus ignores), one iteration and
// DroppedPerTick dropped_iterations; then sleeps TickInterval.
type Config struct {
	Ticks          int
	TickInterval   time.Duration
	ReqsPerTick    int // >= 2: the last request of every tick fails
	DroppedPerTick int
	VUs            int
	// Summary is written to the --summary-export path on a clean finish
	// ("" = no summary).
	Summary  string
	ExitCode int
	// Hold keeps the process alive after streaming (sample pipe still
	// open, as k6 holds it while running) until it is killed.
	Hold bool
}

// Steps are the request names (k6 "name" tag = DSL node id) the fake
// alternates between.
var Steps = [2]string{"home", "product"}

// Sent is the total http_reqs the fake streams.
func (c Config) Sent() int { return c.Ticks * c.ReqsPerTick }

// OK is the total requests the fake streams as not failed.
func (c Config) OK() int { return c.Ticks * (c.ReqsPerTick - 1) }

// Write creates the fake k6 under dir and returns its path.
func Write(dir string, c Config) (string, error) {
	if c.ReqsPerTick < 2 {
		return "", fmt.Errorf("k6fake: ReqsPerTick must be >= 2")
	}
	const ts = "2026-11-16T12:00:00.000000000Z"
	point := func(metric string, value string, tags string) string {
		return `{"metric":"` + metric + `","type":"Point","data":{"time":"` + ts + `","value":` + value + `,"tags":` + tags + `}}`
	}
	decl := func(metric, typ, contains string) string {
		return `{"type":"Metric","data":{"name":"` + metric + `","type":"` + typ + `","contains":"` + contains +
			`","thresholds":[],"submetrics":null},"metric":"` + metric + `"}`
	}
	reqTags := func(step, status string, expected bool) string {
		return `{"expected_response":"` + strconv.FormatBool(expected) + `","group":"","method":"GET","name":"` + step +
			`","proto":"HTTP/1.1","scenario":"workflow","status":"` + status + `","url":"http://target.local/` + step + `"}`
	}
	emit := func(b *strings.Builder, line string) {
		b.WriteString("  printf '%s\\n' '" + line + "' >&3\n")
	}

	var tick strings.Builder
	emit(&tick, point("vus", strconv.Itoa(c.VUs), `{}`))
	for j := 0; j < c.ReqsPerTick; j++ {
		step := Steps[j%2]
		failed := j == c.ReqsPerTick-1
		status, failedVal := "200", "0"
		if failed {
			status, failedVal = "500", "1"
		}
		tags := reqTags(step, status, !failed)
		emit(&tick, point("http_reqs", "1", tags))
		emit(&tick, point("http_req_duration", strconv.Itoa(10*(j+1)), tags))
		emit(&tick, point("http_req_blocked", "0.01", tags))
		emit(&tick, point("http_req_failed", failedVal, tags))
		emit(&tick, point("data_sent", "84", tags))
	}
	iterTags := `{"group":"","scenario":"workflow"}`
	emit(&tick, point("iterations", "1", iterTags))
	emit(&tick, point("iteration_duration", "250", iterTags))
	if c.DroppedPerTick > 0 {
		emit(&tick, point("dropped_iterations", strconv.Itoa(c.DroppedPerTick), `{"scenario":"workflow"}`))
	}

	var decls strings.Builder
	for _, d := range [][3]string{
		{"vus", "gauge", "default"}, {"http_reqs", "counter", "default"},
		{"http_req_duration", "trend", "time"}, {"http_req_failed", "rate", "default"},
		{"iterations", "counter", "default"}, {"dropped_iterations", "counter", "default"},
	} {
		emit(&decls, decl(d[0], d[1], d[2]))
	}

	end := "exec 3>&-\n"
	if c.Hold {
		end = "exec sleep 600\n"
	}
	summary := ""
	if c.Summary != "" {
		summary = `if [ -n "$sum" ]; then printf '%s' '` + c.Summary + `' > "$sum"; fi` + "\n"
	}

	script := `#!/bin/sh
# Fake k6 (see internal/k6fake).
out=""; sum=""
while [ $# -gt 0 ]; do
  case "$1" in
    --summary-export) sum="$2"; shift ;;
    --out) case "$2" in json=*) out="${2#json=}" ;; esac; shift ;;
  esac
  shift
done
echo "          /\      Grafana   /‾‾/  (fake)"
if [ -n "$out" ]; then exec 3>"$out"; else exec 3>/dev/null; fi
{
` + decls.String() + `}
i=0
while [ $i -lt ` + strconv.Itoa(c.Ticks) + ` ]; do
  i=$((i+1))
` + tick.String() + `  sleep ` + fmt.Sprintf("%.3f", c.TickInterval.Seconds()) + `
done
` + end + summary + "exit " + strconv.Itoa(c.ExitCode) + "\n"

	path := filepath.Join(dir, "k6fake.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "", err
	}
	return path, nil
}
