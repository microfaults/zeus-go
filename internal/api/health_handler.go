package api

import (
	"net/http"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/stats"
)

// --- GET /healthz ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- GET /readyz ---

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Ready if the metric registry and stores are initialized.
	// All components are in-memory and available at startup.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- GET /api/v1/status ---

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

func (s *Server) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.computeSummary())
}

// computeSummary builds a stats.Summary from live store counts.
func (s *Server) computeSummary() stats.Summary {
	activeRuns := s.deps.Runs.CountByStatus(run.StatusRunning) +
		s.deps.Runs.CountByStatus(run.StatusStarting) +
		s.deps.Runs.CountByStatus(run.StatusValidating) +
		s.deps.Runs.CountByStatus(run.StatusCompleting)
	activeAttacks := countActiveAttacks(s.deps.Attacks.List())

	return stats.ComputeSummary(activeRuns, activeAttacks, s.deps.Datasets.Count())
}

func countActiveAttacks(attacks []*attacker.Attack) int {
	n := 0
	for _, a := range attacks {
		if a.Status == "running" {
			n++
		}
	}
	return n
}
