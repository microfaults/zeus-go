package stats

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds typed references to all Zeus Prometheus metric families.
type Metrics struct {
	registry *prometheus.Registry

	// Counters
	RunsStartedTotal      *prometheus.CounterVec
	RunsCompletedTotal    *prometheus.CounterVec
	RequestsSentTotal     *prometheus.CounterVec
	RequestsOKTotal       *prometheus.CounterVec
	RequestsDroppedTotal  *prometheus.CounterVec
	VariantPicksTotal     *prometheus.CounterVec
	IterationsTotal       *prometheus.CounterVec
	AttacksStartedTotal   *prometheus.CounterVec
	AttacksHitsTotal      *prometheus.CounterVec
	AttacksMissesTotal    *prometheus.CounterVec
	DatasetsBytesIngested *prometheus.CounterVec

	// Histograms
	RequestDuration       *prometheus.HistogramVec
	StepDuration          *prometheus.HistogramVec
	IterationDuration     *prometheus.HistogramVec
	AttackRequestDuration *prometheus.HistogramVec

	// Gauges
	ActiveRuns       *prometheus.GaugeVec
	ActiveVUs        *prometheus.GaugeVec
	ActiveAttacks    *prometheus.GaugeVec
	DatasetsTotal    *prometheus.GaugeVec
	DatasetSizeBytes *prometheus.GaugeVec
}

// NewMetrics creates a custom Prometheus registry, registers all Zeus metrics,
// and returns the Metrics struct.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		// Counters
		RunsStartedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_runs_started_total",
			Help: "Total number of workflow runs started.",
		}, []string{"workflow_id", "workflow_name"}),

		RunsCompletedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_runs_completed_total",
			Help: "Total number of workflow runs completed.",
		}, []string{"workflow_id", "workflow_name", "status"}),

		RequestsSentTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_requests_sent_total",
			Help: "Total number of requests sent.",
		}, []string{"run_id", "workflow_id", "step_id"}),

		RequestsOKTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_requests_ok_total",
			Help: "Total number of successful requests.",
		}, []string{"run_id", "workflow_id", "step_id"}),

		RequestsDroppedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_requests_dropped_total",
			Help: "Total number of dropped requests.",
		}, []string{"run_id", "workflow_id", "step_id", "reason"}),

		VariantPicksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_variant_picks_total",
			Help: "Total number of variant picks.",
		}, []string{"run_id", "workflow_id", "step_id", "variant_index"}),

		IterationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_iterations_total",
			Help: "Total number of workflow iterations.",
		}, []string{"run_id", "workflow_id"}),

		AttacksStartedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_attacks_started_total",
			Help: "Total number of attacks started.",
		}, []string{"experiment_id"}),

		AttacksHitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_attacks_hits_total",
			Help: "Total number of attack hits.",
		}, []string{"attack_id"}),

		AttacksMissesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_attacks_misses_total",
			Help: "Total number of attack misses.",
		}, []string{"attack_id"}),

		DatasetsBytesIngested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "zeus_datasets_bytes_ingested_total",
			Help: "Total bytes ingested across datasets.",
		}, []string{"dataset_id"}),

		// Histograms
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "zeus_request_duration_seconds",
			Help:    "Duration of individual requests in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"run_id", "workflow_id", "step_id"}),

		StepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "zeus_step_duration_seconds",
			Help: "Duration of workflow steps in seconds.",
		}, []string{"run_id", "workflow_id", "step_id"}),

		IterationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "zeus_iteration_duration_seconds",
			Help: "Duration of workflow iterations in seconds.",
		}, []string{"run_id", "workflow_id"}),

		AttackRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "zeus_attack_request_duration_seconds",
			Help: "Duration of attack requests in seconds.",
		}, []string{"attack_id"}),

		// Gauges
		ActiveRuns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "zeus_active_runs",
			Help: "Number of currently active workflow runs.",
		}, []string{}),

		ActiveVUs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "zeus_active_vus",
			Help: "Number of currently active virtual users.",
		}, []string{"run_id"}),

		ActiveAttacks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "zeus_active_attacks",
			Help: "Number of currently active attacks.",
		}, []string{}),

		DatasetsTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "zeus_datasets_total",
			Help: "Total number of loaded datasets.",
		}, []string{}),

		DatasetSizeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "zeus_dataset_size_bytes",
			Help: "Size of a dataset in bytes.",
		}, []string{"dataset_id"}),
	}

	// Register all metrics with the custom registry.
	reg.MustRegister(
		m.RunsStartedTotal,
		m.RunsCompletedTotal,
		m.RequestsSentTotal,
		m.RequestsOKTotal,
		m.RequestsDroppedTotal,
		m.VariantPicksTotal,
		m.IterationsTotal,
		m.AttacksStartedTotal,
		m.AttacksHitsTotal,
		m.AttacksMissesTotal,
		m.DatasetsBytesIngested,
		m.RequestDuration,
		m.StepDuration,
		m.IterationDuration,
		m.AttackRequestDuration,
		m.ActiveRuns,
		m.ActiveVUs,
		m.ActiveAttacks,
		m.DatasetsTotal,
		m.DatasetSizeBytes,
	)

	return m
}

// Registry returns the underlying Prometheus registry for the scrape handler.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// PruneRunLabels calls DeletePartialMatch on all metric families that carry
// run_id as a label. This is called when a run reaches a terminal state.
func (m *Metrics) PruneRunLabels(runID string) {
	labels := prometheus.Labels{"run_id": runID}

	// Counters with run_id
	m.RequestsSentTotal.DeletePartialMatch(labels)
	m.RequestsOKTotal.DeletePartialMatch(labels)
	m.RequestsDroppedTotal.DeletePartialMatch(labels)
	m.VariantPicksTotal.DeletePartialMatch(labels)
	m.IterationsTotal.DeletePartialMatch(labels)

	// Histograms with run_id
	m.RequestDuration.DeletePartialMatch(labels)
	m.StepDuration.DeletePartialMatch(labels)
	m.IterationDuration.DeletePartialMatch(labels)

	// Gauges with run_id
	m.ActiveVUs.DeletePartialMatch(labels)
}
