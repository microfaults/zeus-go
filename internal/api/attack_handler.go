package api

import (
	"net/http"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/id"
)

// --- POST /api/v1/attacks ---

// handleCreateAttack launches a new vegeta attack.
//
// @Summary      Launch attack
// @Description  Persist and launch a precision attack. The body is an attacker.AttackConfig.
// @Description  When run_ref references an existing run, meta_trace_id and workflow_label are
// @Description  inherited from the linked run if not set on the request. The response is an
// @Description  inline envelope with id, status, and started_at; the launched Attack object is
// @Description  retrievable via GET /attacks/{id}.
// @Tags         attacks
// @Accept       json
// @Produce      json
// @Param        attack  body      attacker.AttackConfig  true  "attack configuration"
// @Success      202     {object}  map[string]any         "attack accepted; envelope contains id, status, started_at"
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

	// If run_ref is supplied, the run must exist — silent miss would defeat
	// atropos's workflow-scoped rule matching (an empty workflow_label flies
	// through cache-box). When run_ref is omitted we accept standalone attacks
	// for now; once the experiment-id flow is enforced end-to-end, this branch
	// should require either run_ref or an explicit standalone-attack opt-in.
	// TODO(experiment-flow): tighten when experiment_id is required.
	if cfg.RunRef != "" {
		rn, ok := s.deps.Runs.Get(cfg.RunRef)
		if !ok {
			writeError(w, http.StatusBadRequest, "run_ref not found: "+cfg.RunRef)
			return
		}
		if cfg.MetaTraceID == "" {
			cfg.MetaTraceID = rn.MetaTraceID
		}
		if cfg.WorkflowLabel == "" {
			cfg.WorkflowLabel = rn.WorkflowLabel
		}
	}

	// onComplete is the single source of truth for the ActiveAttacks gauge:
	// it fires exactly once whether the attack ends naturally, via Stop(), or
	// via the context timeout. handleStopAttack must NOT decrement here.
	onComplete := func(*attacker.Attack) {
		s.deps.Metrics.ActiveAttacks.WithLabelValues().Dec()
	}

	attack, err := s.deps.Attacks.Launch(r.Context(), cfg, onComplete)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Cardinality note: ExperimentID is a label here and the counter is never
	// pruned — manteion is expected to keep experiment ids bounded.
	s.deps.Metrics.AttacksStartedTotal.WithLabelValues(cfg.ExperimentID).Inc()
	s.deps.Metrics.ActiveAttacks.WithLabelValues().Inc()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         attack.Config.ID,
		"status":     attack.Status,
		"started_at": attack.StartedAt,
	})
}

// --- GET /api/v1/attacks ---

// handleListAttacks lists current and historical attacks, optionally filtered.
//
// @Summary      List attacks
// @Description  Returns a bare array of attacks. Optional query filters narrow by status,
// @Description  experiment_id, and run_ref.
// @Tags         attacks
// @Produce      json
// @Param        status         query     string  false  "filter by attack status (e.g. running, completed)"
// @Param        experiment_id  query     string  false  "filter by experiment id"
// @Param        run_ref        query     string  false  "filter by linked run id"
// @Success      200  {array}   attacker.Attack
// @Router       /attacks [get]
func (s *Server) handleListAttacks(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Attacks.List()

	// Filter by query params.
	q := r.URL.Query()
	status := q.Get("status")
	experimentID := q.Get("experiment_id")
	runRef := q.Get("run_ref")

	var filtered []*attacker.Attack
	for _, a := range list {
		if status != "" && a.Status != status {
			continue
		}
		if experimentID != "" && a.Config.ExperimentID != experimentID {
			continue
		}
		if runRef != "" && a.Config.RunRef != runRef {
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
// @Success      200  {object}  attacker.Attack
// @Failure      404  {object}  api.ErrorResponse  "attack not found"
// @Router       /attacks/{id} [get]
func (s *Server) handleGetAttack(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	attack, ok := s.deps.Attacks.Get(attackID)
	if !ok {
		writeError(w, http.StatusNotFound, "attack not found: "+attackID)
		return
	}
	writeJSON(w, http.StatusOK, attack)
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
	attack, ok := s.deps.Attacks.Get(attackID)
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
