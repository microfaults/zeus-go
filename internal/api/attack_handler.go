package api

import (
	"net/http"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/id"
)

// --- POST /api/v1/attacks ---

func (s *Server) handleCreateAttack(w http.ResponseWriter, r *http.Request) {
	var cfg attacker.AttackConfig
	if err := readJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if cfg.ID == "" {
		cfg.ID = id.New()
	}

	// If linked to a run via run_ref, inherit workflow_label and meta_trace_id.
	if cfg.RunRef != "" {
		rn, ok := s.deps.Runs.Get(cfg.RunRef)
		if ok {
			if cfg.MetaTraceID == "" {
				cfg.MetaTraceID = rn.MetaTraceID
			}
			if cfg.WorkflowLabel == "" {
				cfg.WorkflowLabel = rn.WorkflowLabel
			}
		}
	}

	attack, err := s.deps.Attacks.Launch(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.deps.Metrics.AttacksStartedTotal.WithLabelValues(cfg.ExperimentID).Inc()
	s.deps.Metrics.ActiveAttacks.WithLabelValues().Inc()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         attack.Config.ID,
		"status":     attack.Status,
		"started_at": attack.StartedAt,
	})
}

// --- GET /api/v1/attacks ---

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

func (s *Server) handleStopAttack(w http.ResponseWriter, r *http.Request) {
	attackID := r.PathValue("id")
	if err := s.deps.Attacks.Stop(attackID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.deps.Metrics.ActiveAttacks.WithLabelValues().Dec()
	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/attacks/{id}/stats ---

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
	if attack.Config.DedupBypass != "" {
		resp["dedup"] = map[string]any{
			"strategy": attack.Config.DedupBypass,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
