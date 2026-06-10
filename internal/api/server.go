package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/sse"
	"atropos-go/loadgen/internal/stats"
	"atropos-go/loadgen/internal/workflow"
)

const maxBodySize = 1 << 20 // 1 MiB

// Deps groups the dependencies required by the API server.
// Each field is a concrete store or service wired in main().
type Deps struct {
	Workflows *workflow.Store
	Runs      *run.Store
	Datasets  *dataset.Store
	Attacks   *attacker.Manager
	Metrics   *stats.Metrics
	Snapshots *stats.SnapshotStore
	Broker    *sse.Broker
}

// Server holds shared dependencies and configures routing.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// NewServer creates an API server wired to the given components.
func NewServer(d Deps) *Server {
	s := &Server{
		deps: d,
		mux:  http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the underlying http.Handler for use with http.Server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	// Workflows
	s.mux.HandleFunc("POST /api/v1/workflows", s.handleCreateWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows/validate", s.handleValidateWorkflowDoc)
	s.mux.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}", s.handleGetWorkflow)
	s.mux.HandleFunc("DELETE /api/v1/workflows/{id}", s.handleDeleteWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows/{id}/validate", s.handleValidateWorkflow)

	// Runs (workflow-scoped)
	s.mux.HandleFunc("POST /api/v1/workflows/{id}/runs", s.handleCreateRun)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}/runs", s.handleListWorkflowRuns)

	// Runs (cross-workflow)
	s.mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{run_id}", s.handleGetRun)
	s.mux.HandleFunc("DELETE /api/v1/runs/{run_id}", s.handleStopRun)
	s.mux.HandleFunc("GET /api/v1/runs/{run_id}/events", s.handleRunEvents)
	s.mux.HandleFunc("GET /api/v1/runs/{run_id}/stats", s.handleRunStats)

	// Datasets
	s.mux.HandleFunc("POST /api/v1/datasets", s.handleCreateDataset)
	s.mux.HandleFunc("GET /api/v1/datasets", s.handleListDatasets)
	s.mux.HandleFunc("GET /api/v1/datasets/{id}", s.handleGetDataset)
	s.mux.HandleFunc("POST /api/v1/datasets/{id}/upload", s.handleUploadDataset)
	s.mux.HandleFunc("GET /api/v1/datasets/{id}/sample", s.handleSampleDataset)
	s.mux.HandleFunc("DELETE /api/v1/datasets/{id}", s.handleDeleteDataset)

	// Attacks
	s.mux.HandleFunc("POST /api/v1/attacks", s.handleCreateAttack)
	s.mux.HandleFunc("GET /api/v1/attacks", s.handleListAttacks)
	s.mux.HandleFunc("GET /api/v1/attacks/{id}", s.handleGetAttack)
	s.mux.HandleFunc("DELETE /api/v1/attacks/{id}", s.handleStopAttack)
	s.mux.HandleFunc("GET /api/v1/attacks/{id}/stats", s.handleAttackStats)

	// Prometheus metrics
	s.mux.Handle("GET /api/v1/metrics", promhttp.HandlerFor(s.deps.Metrics.Registry(), promhttp.HandlerOpts{}))
	s.mux.HandleFunc("GET /api/v1/metrics/summary", s.handleMetricsSummary)

	// Health. Dual-mounted: root for direct-probe consumers (k8s liveness/readiness),
	// /api/v1/... for spec-driven clients that compose from the server URL — the
	// OpenAPI spec's `servers[0].url` ends in `/api/v1`, so a bare `/healthz` path
	// key composes to `/api/v1/healthz`, which would 404 without the dual mount.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /api/v1/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /api/v1/readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
}

// --- JSON helpers ---

// ErrorResponse is the JSON envelope returned for all error responses.
// swag annotations reference api.ErrorResponse for 4xx/5xx documentation.
//
// Standard shape (subject to future migration to RFC 9457 problem+json
// per manteion-ui/docs/API-NEEDED.md §C.5).
type ErrorResponse struct {
	Error string `json:"error" example:"validation failed"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func readJSON(r *http.Request, v any) error {
	body := io.LimitReader(r.Body, maxBodySize)
	return json.NewDecoder(body).Decode(v)
}
