package api

import (
	"context"
	"net/http"
	"time"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/id"
)

// attackInfo is the GET /attacks/{id} response — field-for-field the shape
// manteion's zeus.AttackInfo decodes. A single-URL vegeta attack has no zeus
// workload or named service, so those fields are empty.
type attackInfo struct {
	ID          string     `json:"id"`
	WorkloadID  string     `json:"workload_id"`
	Service     string     `json:"service"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// attackResultInfo is the GET /attacks/{id}/result response — field-for-field
// the shape manteion's zeus.AttackResultInfo decodes: vegeta totals converted
// to ms / microseconds. Service is empty (a single-URL attack targets a raw
// URL, not a named service); manteion tolerates an empty service.
type attackResultInfo struct {
	AttackID      string  `json:"attack_id"`
	Service       string  `json:"service"`
	TotalRequests int64   `json:"total_requests"`
	DurationMs    int64   `json:"duration_ms"`
	RateActual    float64 `json:"rate_actual"`
	SuccessRate   float64 `json:"success_rate"`
	LatencyP50Us  int64   `json:"latency_p50_us"`
	LatencyP90Us  int64   `json:"latency_p90_us"`
	LatencyP95Us  int64   `json:"latency_p95_us"`
	LatencyP99Us  int64   `json:"latency_p99_us"`
	ThroughputRPS float64 `json:"throughput_rps"`
}

// --- POST /api/v1/attacks ---

// handleCreateAttack launches a new vegeta attack.
//
// @Summary      Launch attack
// @Description  Persist and launch a precision attack. The body is an attacker.AttackConfig.
// @Description  The response is an inline envelope with id, status, and started_at; the launched
// @Description  Attack is retrievable via GET /attacks/{id}, and its final metrics via
// @Description  GET /attacks/{id}/result once it completes.
// @Tags         attacks
// @Accept       json
// @Produce      json
// @Param        attack  body      attacker.AttackConfig  true  "attack configuration"
// @Success      201     {object}  map[string]any         "attack created; envelope contains id, status, started_at"
// @Failure      400     {object}  api.ErrorResponse      "invalid JSON or attack validation error"
// @Router       /attacks [post]
func (s *Server) handleCreateAttack(w http.ResponseWriter, r *http.Request) {
	var cfg attacker.AttackConfig
	if err := readJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if cfg.ID == "" {
		cfg.ID = id.New()
	}

	// onComplete is the single source of truth for the ActiveAttacks gauge:
	// it fires exactly once whether the attack ends naturally, via Stop(), or
	// via the context timeout. handleStopAttack must NOT decrement here.
	onComplete := func(*attacker.Attack) {
		s.deps.Metrics.ActiveAttacks.WithLabelValues().Dec()
	}

	// Detach from the request context: the attack outlives the HTTP response,
	// so binding it to r.Context() would cancel vegeta the instant this handler
	// returns and finalize every attack as "stopped". WithoutCancel keeps
	// request-scoped values (trace) but drops the cancellation signal.
	attack, err := s.deps.Attacks.Launch(context.WithoutCancel(r.Context()), cfg, onComplete)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Cardinality note: ExperimentID is a label here and the counter is never
	// pruned — manteion is expected to keep experiment ids bounded.
	s.deps.Metrics.AttacksStartedTotal.WithLabelValues(cfg.ExperimentID).Inc()
	s.deps.Metrics.ActiveAttacks.WithLabelValues().Inc()

	// 201 Created (not 202 Accepted): manteion's StartAttack accepts only 200/201.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         attack.Config.ID,
		"status":     attack.Status,
		"started_at": attack.StartedAt,
	})
}

// --- GET /api/v1/attacks ---

// handleListAttacks lists current and historical attacks, optionally filtered.
//
// @Summary      List attacks
// @Description  Returns a bare array of attacks. Optional query filters narrow by status
// @Description  and experiment_id.
// @Tags         attacks
// @Produce      json
// @Param        status         query     string  false  "filter by attack status (e.g. running, completed)"
// @Param        experiment_id  query     string  false  "filter by experiment id"
// @Success      200  {array}   attacker.Attack
// @Router       /attacks [get]
func (s *Server) handleListAttacks(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Attacks.ViewAll()

	// Filter by query params.
	q := r.URL.Query()
	status := q.Get("status")
	experimentID := q.Get("experiment_id")

	var filtered []attacker.Attack
	for _, a := range list {
		if status != "" && a.Status != status {
			continue
		}
		if experimentID != "" && a.Config.ExperimentID != experimentID {
			continue
		}
		filtered = append(filtered, a)
	}

	writeJSON(w, http.StatusOK, filtered)
}

// --- GET /api/v1/attacks/{id} ---

// handleGetAttack returns a single attack by id.
//
// @Summary      Get attack
// @Tags         attacks
// @Produce      json
// @Param        id   path      string  true  "Attack ID"
// @Success      200  {object}  api.attackInfo
// @Failure      404  {object}  api.ErrorResponse  "attack not found"
// @Router       /attacks/{id} [get]
func (s *Server) handleGetAttack(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	attack, ok := s.deps.Attacks.View(attackID)
	if !ok {
		writeError(w, http.StatusNotFound, "attack not found: "+attackID)
		return
	}
	started := attack.StartedAt
	writeJSON(w, http.StatusOK, attackInfo{
		ID:          attack.Config.ID,
		Status:      attack.Status,
		StartedAt:   &started,
		CompletedAt: attack.CompletedAt,
	})
}

// --- GET /api/v1/attacks/{id}/result ---

// handleAttackResult returns the final metrics for a completed attack.
//
// @Summary      Get attack result
// @Description  Final vegeta metrics for a completed attack, normalized to manteion's
// @Description  AttackResultInfo (duration in ms, latency percentiles in microseconds).
// @Description  Returns 404 while the attack is still running (no result yet) so callers can
// @Description  poll; the body becomes available once the attack finalizes.
// @Tags         attacks
// @Produce      json
// @Param        id   path      string  true  "Attack ID"
// @Success      200  {object}  api.attackResultInfo
// @Failure      404  {object}  api.ErrorResponse  "attack not found, or result not ready yet"
// @Router       /attacks/{id}/result [get]
func (s *Server) handleAttackResult(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	attack, ok := s.deps.Attacks.View(attackID)
	if !ok {
		writeError(w, http.StatusNotFound, "attack not found: "+attackID)
		return
	}
	// 404 == "not ready yet" in manteion's contract: GetAttackResult polls until
	// the result materializes. An attack with no Result has not finalized.
	if attack.Result == nil {
		writeError(w, http.StatusNotFound, "attack result not ready: "+attackID)
		return
	}
	writeJSON(w, http.StatusOK, toAttackResultInfo(attackID, attack.Result))
}

// toAttackResultInfo maps zeus's vegeta-derived AttackResult to the wire shape
// manteion decodes. Service is empty: a single-URL attack has no named service.
// ThroughputRPS uses vegeta's achieved request rate (manteion falls back to
// requests/duration when zero, so either is acceptable).
func toAttackResultInfo(attackID string, res *attacker.AttackResult) attackResultInfo {
	return attackResultInfo{
		AttackID:      attackID,
		TotalRequests: int64(res.TotalRequests),
		DurationMs:    res.Duration.Milliseconds(),
		RateActual:    res.RateActual,
		SuccessRate:   res.Success,
		LatencyP50Us:  res.Latencies.P50.Microseconds(),
		LatencyP90Us:  res.Latencies.P90.Microseconds(),
		LatencyP95Us:  res.Latencies.P95.Microseconds(),
		LatencyP99Us:  res.Latencies.P99.Microseconds(),
		ThroughputRPS: res.RateActual,
	}
}

// --- DELETE /api/v1/attacks/{id} ---

// handleStopAttack cancels an in-flight attack.
//
// @Summary      Stop attack
// @Description  Cancels a running attack and decrements the active-attacks gauge.
// @Tags         attacks
// @Produce      json
// @Param        id   path      string  true  "Attack ID"
// @Success      204  "attack stopped"
// @Failure      404  {object}  api.ErrorResponse  "attack not found"
// @Router       /attacks/{id} [delete]
func (s *Server) handleStopAttack(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	if err := s.deps.Attacks.Stop(attackID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// ActiveAttacks is decremented in the launch goroutine's onComplete hook,
	// not here — Stop() halts vegeta and the goroutine finalizes the gauge.
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/attacks/{id}/stats ---

// handleAttackStats returns per-attack stats including dedup metrics.
//
// @Summary      Get attack stats
// @Description  Returns an inline envelope with attack_id, status, config, optional result
// @Description  (vegeta-derived totals + latency percentiles), and optional dedup strategy
// @Description  metadata when DedupBypass is configured on the attack.
// @Tags         attacks
// @Produce      json
// @Param        id   path      string          true  "Attack ID"
// @Success      200  {object}  map[string]any  "attack stats envelope (attack_id, status, config, result?, dedup?)"
// @Failure      404  {object}  api.ErrorResponse  "attack not found"
// @Router       /attacks/{id}/stats [get]
func (s *Server) handleAttackStats(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	attack, ok := s.deps.Attacks.View(attackID)
	if !ok {
		writeError(w, http.StatusNotFound, "attack not found: "+attackID)
		return
	}

	resp := map[string]any{
		"attack_id": attackID,
		"status":    attack.Status,
		"config":    attack.Config,
	}
	if attack.Result != nil {
		resp["result"] = attack.Result
	}
	// TODO(metrics): the dedup envelope below only echoes the strategy that
	// was requested. When per-attack dedup-effectiveness metrics land
	// (unique-key count, collision rate, distribution of mutated values),
	// surface them here so callers can verify the bypass actually defeated
	// the SUT's dedup. The emission point is the targeter in
	// internal/attacker/attacker.go.
	if attack.Config.DedupBypass != nil {
		resp["dedup"] = map[string]any{
			"strategy": attack.Config.DedupBypass.Strategy,
			"source":   attack.Config.DedupBypass.Source,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
