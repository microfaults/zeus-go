package api

import (
	"net/http"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/stats"
)

// --- GET /healthz ---

// handleHealthz is a process-up liveness probe.
//
// @Summary      Liveness probe
// @Description  Returns 200 with {status: ok} once the process accepts requests. Dual-mounted
// @Description  at root (/healthz) and /api/v1/healthz; the @Router uses the bare path key
// @Description  so spec consumers composing servers[0].url + path resolve correctly.
// @Tags         health
// @Produce      json
// @Success      200  {object}  map[string]string  "status envelope"
// @Router       /healthz [get]
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- GET /readyz ---

// handleReadyz is the readiness probe.
//
// @Summary      Readiness probe
// @Description  Returns 200 with {status: ready} when the metric registry and in-memory stores
// @Description  are initialized. Dual-mounted at root (/readyz) and /api/v1/readyz.
// @Tags         health
// @Produce      json
// @Success      200  {object}  map[string]string  "status envelope"
// @Router       /readyz [get]
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Ready if the metric registry and stores are initialized.
	// All components are in-memory and available at startup.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- GET /api/v1/status ---

// handleStatus returns an operator-friendly system overview envelope.
//
// @Summary      Operator status overview
// @Description  Returns active_runs, active_attacks, datasets, and uptime_seconds plus a
// @Description  status string. Computed from live store counts on every call.
// @Tags         health
// @Produce      json
// @Success      200  {object}  map[string]any  "status overview envelope"
// @Router       /status [get]
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	summary := s.computeSummary()

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"active_runs":    summary.ActiveRuns,
		"active_attacks": summary.ActiveAttacks,
		"datasets":       summary.Datasets,
		"uptime_seconds": summary.UptimeSeconds,
	})
}

// --- GET /api/v1/metrics/summary ---

// handleMetricsSummary returns a Zeus-wide summary of live counts.
//
// @Summary      Zeus-wide stats summary
// @Description  Returns a stats.Summary with active_runs, active_attacks, datasets, and
// @Description  uptime_seconds. Companion to GET /metrics (Prometheus scrape) for callers
// @Description  that don't speak Prometheus.
// @Tags         metrics
// @Produce      json
// @Success      200  {object}  stats.Summary
// @Router       /metrics/summary [get]
func (s *Server) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.computeSummary())
}

// computeSummary builds a stats.Summary from live store counts.
func (s *Server) computeSummary() stats.Summary {
	activeRuns := s.deps.Runs.CountByStatus(run.StatusRunning) +
		s.deps.Runs.CountByStatus(run.StatusStarting) +
		s.deps.Runs.CountByStatus(run.StatusValidating) +
		s.deps.Runs.CountByStatus(run.StatusCompleting)
	// ViewAll, not List: List hands out the live *Attack pointers, and the
	// launch goroutine writes Status/Result under the manager lock the
	// reader here doesn't hold -- a confirmed -race data race. ViewAll
	// copies under the lock.
	activeAttacks := countActiveAttacks(s.deps.Attacks.ViewAll())

	return stats.ComputeSummary(activeRuns, activeAttacks, s.deps.Datasets.Count())
}

func countActiveAttacks(attacks []attacker.Attack) int {
	n := 0
	for _, a := range attacks {
		if a.Status == "running" {
			n++
		}
	}
	return n
}
