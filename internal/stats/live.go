package stats

import (
	"math"
	"sync"
	"time"
)

// k6 metric names LiveRun understands. Every other k6 metric is ignored.
const (
	K6HTTPReqs          = "http_reqs"
	K6HTTPReqFailed     = "http_req_failed"
	K6HTTPReqDuration   = "http_req_duration"
	K6Iterations        = "iterations"
	K6IterationDuration = "iteration_duration"
	K6DroppedIterations = "dropped_iterations"
	K6VUs               = "vus"
)

// DropReasonDroppedIteration is the zeus_requests_dropped_total reason label
// for k6 dropped_iterations (arrivals the open-loop executor skipped).
const DropReasonDroppedIteration = "dropped_iteration"

// IsLiveMetric reports whether LiveRun.Observe uses samples of the named k6
// metric, so a stream reader can skip the rest without decoding them.
func IsLiveMetric(name string) bool {
	switch name {
	case K6HTTPReqs, K6HTTPReqFailed, K6HTTPReqDuration, K6Iterations,
		K6IterationDuration, K6DroppedIterations, K6VUs:
		return true
	}
	return false
}

// LiveRun aggregates one run's k6 sample stream while the run is in flight.
// Each sample updates running totals and a fixed-size latency sketch, and is
// forwarded to the run's Prometheus series; no sample is retained, so memory
// stays constant however long the run is. Safe for concurrent use.
type LiveRun struct {
	runID      string
	workflowID string
	startedAt  time.Time
	metrics    *Metrics // nil = no Prometheus feed

	mu         sync.Mutex
	iterations float64
	sent       float64
	ok         float64
	dropped    float64
	activeVUs  float64
	latency    latencySketch
}

// NewLiveRun starts aggregating for runID. m may be nil.
func NewLiveRun(m *Metrics, runID, workflowID string, startedAt time.Time) *LiveRun {
	return &LiveRun{runID: runID, workflowID: workflowID, startedAt: startedAt, metrics: m}
}

// Observe ingests one k6 sample: metric is the k6 metric name, value is in
// k6's unit (milliseconds for durations), step is the sample's k6 "name" tag
// (the DSL request node id; only meaningful for http_* metrics).
func (lr *LiveRun) Observe(metric string, value float64, step string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	m := lr.metrics
	switch metric {
	case K6HTTPReqs:
		if value < 0 {
			return
		}
		lr.sent += value
		if m != nil {
			m.RequestsSentTotal.WithLabelValues(lr.runID, lr.workflowID, step).Add(value)
		}
	case K6HTTPReqFailed:
		// Rate metric: 1 = the request failed, 0 = it did not.
		if value != 0 {
			return
		}
		lr.ok++
		if m != nil {
			m.RequestsOKTotal.WithLabelValues(lr.runID, lr.workflowID, step).Inc()
		}
	case K6HTTPReqDuration:
		if value < 0 {
			return
		}
		lr.latency.add(value)
		if m != nil {
			m.RequestDuration.WithLabelValues(lr.runID, lr.workflowID, step).Observe(value / 1000)
		}
	case K6Iterations:
		if value < 0 {
			return
		}
		lr.iterations += value
		if m != nil {
			m.IterationsTotal.WithLabelValues(lr.runID, lr.workflowID).Add(value)
		}
	case K6IterationDuration:
		if value < 0 {
			return
		}
		if m != nil {
			m.IterationDuration.WithLabelValues(lr.runID, lr.workflowID).Observe(value / 1000)
		}
	case K6DroppedIterations:
		if value < 0 {
			return
		}
		lr.dropped += value
		if m != nil {
			m.RequestsDroppedTotal.WithLabelValues(lr.runID, lr.workflowID, "", DropReasonDroppedIteration).Add(value)
		}
	case K6VUs:
		lr.activeVUs = value
		if m != nil {
			m.ActiveVUs.WithLabelValues(lr.runID).Set(value)
		}
	}
}

// End marks the run over: the active-VU gauge drops to 0 (the counters and
// histograms keep their final values until the run is evicted).
func (lr *LiveRun) End() {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.activeVUs = 0
	if lr.metrics != nil {
		lr.metrics.ActiveVUs.WithLabelValues(lr.runID).Set(0)
	}
}

// Snapshot renders the totals so far as a RunStats with Source
// SourceStream. final marks a terminal snapshot (FinalizedAt set).
func (lr *LiveRun) Snapshot(status string, final bool) *RunStats {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	now := time.Now()
	ms := func(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }
	rs := &RunStats{
		RunID:           lr.runID,
		WorkflowID:      lr.workflowID,
		Status:          status,
		Final:           final,
		Source:          SourceStream,
		Duration:        now.Sub(lr.startedAt),
		Iterations:      int64(lr.iterations),
		RequestsSent:    int64(lr.sent),
		RequestsOK:      int64(lr.ok),
		RequestsDropped: int64(lr.dropped),
		ActiveVUs:       int64(lr.activeVUs),
		LatencyP50:      ms(lr.latency.quantile(0.50)),
		LatencyP95:      ms(lr.latency.quantile(0.95)),
		LatencyP99:      ms(lr.latency.quantile(0.99)),
		UpdatedAt:       now,
	}
	if final {
		rs.FinalizedAt = &now
	}
	return rs
}

// latencySketch is a log-bucketed histogram over milliseconds: bucket i > 0
// covers [sketchMinMs*g^(i-1), sketchMinMs*g^i) with g = 1.02, so a quantile
// read back from it is within ~1% of the true sample value. Fixed size
// (~10 KiB) regardless of how many samples it absorbs.
type latencySketch struct {
	counts   [sketchBuckets]uint64
	total    uint64
	min, max float64
}

const (
	sketchMinMs   = 0.001 // 1µs; everything at or below lands in bucket 0
	sketchGrowth  = 1.02
	sketchBuckets = 1200 // top bucket starts at ~5.8h
)

var sketchLogGrowth = math.Log(sketchGrowth)

func (s *latencySketch) add(v float64) {
	if s.total == 0 || v < s.min {
		s.min = v
	}
	if s.total == 0 || v > s.max {
		s.max = v
	}
	s.total++
	i := 0
	if v > sketchMinMs {
		i = int(math.Log(v/sketchMinMs)/sketchLogGrowth) + 1
		if i >= sketchBuckets {
			i = sketchBuckets - 1
		}
	}
	s.counts[i]++
}

// quantile returns the q-th quantile (0..1) in milliseconds, 0 when empty.
func (s *latencySketch) quantile(q float64) float64 {
	if s.total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(s.total)))
	if rank < 1 {
		rank = 1
	}
	var seen uint64
	for i, c := range s.counts {
		seen += c
		if seen < rank {
			continue
		}
		v := sketchMinMs
		if i > 0 {
			// Geometric midpoint of the bucket bounds.
			v = sketchMinMs * math.Pow(sketchGrowth, float64(i)-0.5)
		}
		return math.Min(math.Max(v, s.min), s.max)
	}
	return s.max
}
