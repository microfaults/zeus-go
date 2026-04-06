package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"atropos-go/loadgen/internal/workload"
)

func (s *Server) handleCreateWorkload(w http.ResponseWriter, r *http.Request) {
	var wl workload.Workload
	if err := readJSON(r, &wl); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if wl.ID == "" {
		wl.ID = generateID()
	}
	if wl.Status == "" {
		wl.Status = "running"
	}
	wl.RegisteredAt = time.Now()

	if err := s.registry.Register(&wl); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, wl)
}

func (s *Server) handleListWorkloads(w http.ResponseWriter, r *http.Request) {
	list := s.registry.List()
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetWorkload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wl, ok := s.registry.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "workload not found")
		return
	}
	writeJSON(w, http.StatusOK, wl)
}

func (s *Server) handleDeleteWorkload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.registry.Deregister(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateWorkload handles PATCH /api/v1/workloads/{id}.
// Manteion uses this to pause, resume, or stop a running k6 flow.
// The k6 sidecar polls its own workload entry and self-governs based on status.
func (s *Server) handleUpdateWorkload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var patch struct {
		Status string `json:"status"`
	}
	if err := readJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	switch patch.Status {
	case "running", "paused", "stopped":
		// valid
	default:
		writeError(w, http.StatusBadRequest,
			`status must be one of: "running", "paused", "stopped"`)
		return
	}

	if err := s.registry.UpdateStatus(id, patch.Status); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	wl, _ := s.registry.Get(id)
	writeJSON(w, http.StatusOK, wl)
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
