package api

import (
	"net/http"
	"time"

	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/workflow"
)

// --- POST /api/v1/workflows ---

type createWorkflowRequest struct {
	Workflow  *workflow.Workflow `json:"workflow"`
	Overwrite bool              `json:"overwrite"`
}

type createWorkflowResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) handleCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req createWorkflowRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Workflow == nil {
		writeError(w, http.StatusBadRequest, "workflow field is required")
		return
	}

	wf := req.Workflow
	if err := workflow.ValidateWorkflow(wf); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := workflow.ValidateSchema(wf); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Handle overwrite: if name exists and overwrite is true, delete the old one first.
	if req.Overwrite {
		if existing, ok := s.deps.Workflows.GetByName(wf.Name); ok {
			_ = s.deps.Workflows.Delete(existing.ID)
		}
	}

	wf.CreatedAt = time.Now()
	if err := s.deps.Workflows.Register(wf); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, createWorkflowResponse{
		ID:        wf.ID,
		Name:      wf.Name,
		Version:   wf.Version,
		CreatedAt: wf.CreatedAt,
	})
}

// --- GET /api/v1/workflows ---

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Workflows.List()
	type item struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Version string   `json:"version"`
		Targets []string `json:"targets"`
	}
	items := make([]item, len(list))
	for i, wf := range list {
		items[i] = item{
			ID:      wf.ID,
			Name:    wf.Name,
			Version: wf.Version,
			Targets: wf.Targets,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": items})
}

// --- GET /api/v1/workflows/{id} ---

func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wf, ok := s.deps.Workflows.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "workflow not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

// --- DELETE /api/v1/workflows/{id} ---

func (s *Server) handleDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Check for active runs referencing this workflow.
	active := s.deps.Runs.List(runListFilters("", "", id))
	for _, rn := range active {
		if !run.IsTerminal(rn.Status) {
			writeError(w, http.StatusConflict, "workflow has active runs; stop them first")
			return
		}
	}

	if err := s.deps.Workflows.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /api/v1/workflows/{id}/validate ---

type validateRequest struct {
	DatasetID string `json:"dataset_id"`
}

func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wf, ok := s.deps.Workflows.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "workflow not found: "+id)
		return
	}
	if wf.DataSchema == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warnings": []string{}})
		return
	}

	var req validateRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	ds, ok := s.deps.Datasets.Get(req.DatasetID)
	if !ok {
		writeError(w, http.StatusNotFound, "dataset not found: "+req.DatasetID)
		return
	}

	result := dataset.ValidateDataset(ds, wf.DataSchema)
	if result.OK {
		writeJSON(w, http.StatusOK, result)
	} else {
		writeJSON(w, http.StatusUnprocessableEntity, result)
	}
}

