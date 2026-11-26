package stats

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// TestLatencySketch_Accuracy: quantiles read from the fixed-size sketch stay
// within ~1% of the exact order statistic across a wide latency range.
func TestLatencySketch_Accuracy(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func(r *rand.Rand) float64
	}{
		{"uniform 1-100ms", func(r *rand.Rand) float64 { return 1 + 99*r.Float64() }},
		{"lognormal tail", func(r *rand.Rand) float64 { return math.Exp(3 + 1.5*r.NormFloat64()) }},
		{"sub-millisecond", func(r *rand.Rand) float64 { return 0.05 + 0.5*r.Float64() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rand.New(rand.NewSource(1))
			var s latencySketch
			vals := make([]float64, 50_000)
			for i := range vals {
				vals[i] = tc.gen(r)
				s.add(vals[i])
			}
			sort.Float64s(vals)
			for _, q := range []float64{0.5, 0.9, 0.95, 0.99} {
				exact := vals[int(math.Ceil(q*float64(len(vals))))-1]
				got := s.quantile(q)
				if rel := math.Abs(got-exact) / exact; rel > 0.011 {
					t.Errorf("p%v = %v, exact %v (rel err %.4f)", q*100, got, exact, rel)
				}
			}
		})
	}
	var empty latencySketch
	if empty.quantile(0.99) != 0 {
		t.Fatal("empty sketch quantile must be 0")
	}
}

// TestLiveRun_FeedsDeclaredSeries: each k6 metric lands in its declared
// family with exactly the declared labels; End zeroes the VU gauge;
// PruneRunLabels (run eviction) removes every per-run series.
func TestLiveRun_FeedsDeclaredSeries(t *testing.T) {
	m := NewMetrics()
	lr := NewLiveRun(m, "r1", "wf1", time.Now())
	lr.Observe(K6HTTPReqs, 1, "home")
	lr.Observe(K6HTTPReqs, 1, "cart")
	lr.Observe(K6HTTPReqFailed, 0, "home")
	lr.Observe(K6HTTPReqFailed, 1, "cart")
	lr.Observe(K6HTTPReqDuration, 25, "home")
	lr.Observe(K6Iterations, 1, "")
	lr.Observe(K6IterationDuration, 300, "")
	lr.Observe(K6DroppedIterations, 2, "")
	lr.Observe(K6VUs, 9, "")
	lr.Observe("http_req_blocked", 5, "home") // ignored

	if v := value(t, m, "zeus_requests_sent_total", "r1", "wf1", "home"); v != 1 {
		t.Errorf("sent{home} = %v", v)
	}
	if v := value(t, m, "zeus_requests_ok_total", "r1", "wf1", "home"); v != 1 {
		t.Errorf("ok{home} = %v", v)
	}
	if n := count(t, m, "zeus_requests_ok_total"); n != 1 {
		t.Errorf("ok series = %d, want 1 (the failed cart request is not ok)", n)
	}
	if v := value(t, m, "zeus_requests_dropped_total", "r1", "wf1", "", DropReasonDroppedIteration); v != 2 {
		t.Errorf("dropped = %v", v)
	}
	if v := value(t, m, "zeus_iterations_total", "r1", "wf1"); v != 1 {
		t.Errorf("iterations = %v", v)
	}
	if v := value(t, m, "zeus_active_vus", "r1"); v != 9 {
		t.Errorf("vus = %v", v)
	}
	if n := count(t, m, "zeus_request_duration_seconds"); n != 1 {
		t.Errorf("request duration series = %d", n)
	}
	if n := count(t, m, "zeus_iteration_duration_seconds"); n != 1 {
		t.Errorf("iteration duration series = %d", n)
	}

	rs := lr.Snapshot("running", false)
	if rs.RequestsSent != 2 || rs.RequestsOK != 1 || rs.RequestsDropped != 2 || rs.Iterations != 1 ||
		rs.ActiveVUs != 9 || rs.Final || rs.FinalizedAt != nil || rs.Source != SourceStream {
		t.Fatalf("interim snapshot = %+v", rs)
	}
	if rs.LatencyP50 < 24750*time.Microsecond || rs.LatencyP50 > 25250*time.Microsecond {
		t.Fatalf("p50 = %v, want ~25ms", rs.LatencyP50)
	}

	lr.End()
	if v := value(t, m, "zeus_active_vus", "r1"); v != 0 {
		t.Errorf("vus after End = %v", v)
	}
	if fs := lr.Snapshot("stopped", true); !fs.Final || fs.FinalizedAt == nil || fs.ActiveVUs != 0 {
		t.Fatalf("final snapshot = %+v", fs)
	}

	m.PruneRunLabels("r1")
	for _, n := range []int{
		count(t, m, "zeus_requests_sent_total"),
		count(t, m, "zeus_requests_ok_total"),
		count(t, m, "zeus_requests_dropped_total"),
		count(t, m, "zeus_iterations_total"),
		count(t, m, "zeus_request_duration_seconds"),
		count(t, m, "zeus_iteration_duration_seconds"),
		count(t, m, "zeus_active_vus"),
	} {
		if n != 0 {
			t.Fatalf("series left after PruneRunLabels: %d", n)
		}
	}
}

// series returns the gathered series of one metric family.
func series(t *testing.T, m *Metrics, family string) []*dto.Metric {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == family {
			return mf.GetMetric()
		}
	}
	return nil
}

func count(t *testing.T, m *Metrics, family string) int {
	t.Helper()
	return len(series(t, m, family))
}

// value returns the counter/gauge value of the family's series whose label
// values (in label-name order) are want.
func value(t *testing.T, m *Metrics, family string, want ...string) float64 {
	t.Helper()
	for _, mt := range series(t, m, family) {
		lps := mt.GetLabel() // sorted by label name
		vals := map[string]string{}
		for _, lp := range lps {
			vals[lp.GetName()] = lp.GetValue()
		}
		if matchDeclared(family, vals, want) {
			if mt.Counter != nil {
				return mt.GetCounter().GetValue()
			}
			return mt.GetGauge().GetValue()
		}
	}
	t.Fatalf("%s%v: no such series", family, want)
	return 0
}

// declaredOrder is each family's label order as declared in NewMetrics
// (the order WithLabelValues takes).
var declaredOrder = map[string][]string{
	"zeus_requests_sent_total":    {"run_id", "workflow_id", "step_id"},
	"zeus_requests_ok_total":      {"run_id", "workflow_id", "step_id"},
	"zeus_requests_dropped_total": {"run_id", "workflow_id", "step_id", "reason"},
	"zeus_iterations_total":       {"run_id", "workflow_id"},
	"zeus_active_vus":             {"run_id"},
}

func matchDeclared(family string, vals map[string]string, want []string) bool {
	names := declaredOrder[family]
	if len(names) != len(want) || len(vals) != len(names) {
		return false
	}
	for i, n := range names {
		if v, ok := vals[n]; !ok || v != want[i] {
			return false
		}
	}
	return true
}
