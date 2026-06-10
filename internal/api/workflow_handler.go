package api

import (
	"net/http"
	"time"

	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/workflow"
)

// --- POST /api/v1/workflows/validate ---

// validateWorkflowDocRequest is the stateless-validation envelope: the same
// `workflow` field as createWorkflowRequest, without overwrite (nothing is
// registered).
type validateWorkflowDocRequest struct {
	Workflow *workflow.Workflow `json:"workflow"`
}

// handleValidateWorkflowDoc runs a DSL v2 document through the same
// validators as workflow registration WITHOUT touching the store — no id
// assignment, no registration. This is the validation authority manteion
// calls at workflow create/update time: manteion owns the durable
// definition, zeus owns DSL semantics.
//
// @Summary      Validate workflow document (stateless)
// @Description  Validates a DSL v2 document (structure, version, targets,
// @Description  node/variant caps) without registering it. 200 {ok,name} on
// @Description  success; 400 with the validator message otherwise.
// @Tags         workflows
// @Accept       json
// @Produce      json
// @Param        body  body      api.validateWorkflowDocRequest  true  "workflow envelope"
// @Success      200   {object}  map[string]any     "{ok: true, name: ...}"
// @Failure      400   {object}  api.ErrorResponse  "invalid JSON or workflow validation error"
// @Router       /workflows/validate [post]
func (s *Server) handleValidateWorkflowDoc(w http.ResponseWriter, r *http.Request) {
	var req validateWorkflowDocRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Workflow == nil {
		writeError(w, http.StatusBadRequest, "workflow field is required")
		return
	}
	if err := workflow.ValidateWorkflow(req.Workflow); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := workflow.ValidateSchema(req.Workflow); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Workflow.Name})
}

// --- POST /api/v1/workflows ---

type createWorkflowRequest struct {
	Workflow  *workflow.Workflow `json:"workflow"`
	Overwrite bool               `json:"overwrite"`
}

type createWorkflowResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// handleCreateWorkflow registers a workflow DSL v2 document.
//
// @Summary      Register workflow
// @Description  Persist a Zeus DSL v2 workflow. The body envelope wraps the workflow object
// @Description  alongside an overwrite flag; when overwrite is true, an existing workflow with
// @Description  the same name is replaced. Schema validation runs before persistence; failures
// @Description  return 400 with the validator error string. Name conflicts without overwrite
// @Description  yield 409.
// @Tags         workflows
// @Accept       json
// @Produce      json
// @Param        body  body      api.createWorkflowRequest   true  "workflow envelope"
// @Success      201   {object}  api.createWorkflowResponse
// @Failure      400   {object}  api.ErrorResponse  "invalid JSON or workflow validation error"
// @Failure      409   {object}  api.ErrorResponse  "workflow name conflict (overwrite != true)"
// @Router       /workflows [post]
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

// handleListWorkflows returns a wrapper containing the registered workflows summary list.
//
// @Summary      List workflows
// @Description  Returns an envelope { "workflows": [...] } where each entry is a compact
// @Description  summary (id, name, version, targets) — not the full DSL document. Use GET
// @Description  /workflows/{id} for the full document.
// @Tags         workflows
// @Produce      json
// @Success      200  {object}  map[string]any  "envelope with workflows array"
// @Router       /workflows [get]
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

// handleGetWorkflow fetches a workflow's full DSL document.
//
// @Summary      Get workflow
// @Tags         workflows
// @Produce      json
// @Param        id   path      string  true  "Workflow ID"
// @Success      200  {object}  workflow.Workflow
// @Failure      404  {object}  api.ErrorResponse  "workflow not found"
// @Router       /workflows/{id} [get]
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

// handleDeleteWorkflow removes a workflow if no active runs reference it.
//
// @Summary      Delete workflow
// @Description  Refuses with 409 when any non-terminal run is still bound to the workflow;
// @Description  callers should stop those runs first.
// @Tags         workflows
// @Produce      json
// @Param        id   path      string  true  "Workflow ID"
// @Success      204  "workflow deleted"
// @Failure      404  {object}  api.ErrorResponse  "workflow not found"
// @Failure      409  {object}  api.ErrorResponse  "workflow has active runs"
// @Router       /workflows/{id} [delete]
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

// handleValidateWorkflow validates a dataset against a workflow's data schema.
//
// @Summary      Validate workflow against dataset
// @Description  Runs ValidateDataset against the workflow's DataSchema. Workflows without a
// @Description  DataSchema short-circuit to {ok: true, warnings: []}. The result body is a
// @Description  dataset.ValidationResult. Returns 422 (not 400) when the dataset fails schema
// @Description  validation, mirroring run-create's rejected semantics. 400 is reserved for
// @Description  malformed request bodies.
// @Tags         workflows
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Workflow ID"
// @Param        body  body      api.validateRequest     true  "validation request (dataset id)"
// @Success      200   {object}  dataset.ValidationResult  "validation succeeded"
// @Failure      400   {object}  api.ErrorResponse        "invalid JSON"
// @Failure      404   {object}  api.ErrorResponse        "workflow or dataset not found"
// @Failure      422   {object}  dataset.ValidationResult "dataset failed schema validation"
// @Router       /workflows/{id}/validate [post]
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
