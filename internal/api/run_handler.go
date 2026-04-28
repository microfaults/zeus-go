package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/id"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/sse"
)

// --- POST /api/v1/workflows/{id}/runs ---

type createRunRequest struct {
	RunID         string            `json:"run_id"`
	ExperimentID  string            `json:"experiment_id"`
	DatasetID     string            `json:"dataset_id"`
	DatasetInline map[string]any    `json:"dataset_inline"`
	VUs           int               `json:"vus"`
	DurationS     int               `json:"duration_s"`
	Persona       string            `json:"persona"`
	MetaTraceID   string            `json:"meta_trace_id"`
	Labels        map[string]string `json:"labels"`
	WorkflowLabel string            `json:"workflow_label"`
}

type createRunResponse struct {
	RunID       string    `json:"run_id"`
	Status      string    `json:"status"`
	MetaTraceID string    `json:"meta_trace_id"`
	K6JobName   string    `json:"k6_job_name"`
	StartedAt   time.Time `json:"started_at"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("id")
	wf, ok := s.deps.Workflows.Get(workflowID)
	if !ok {
		writeError(w, http.StatusNotFound, "workflow not found: "+workflowID)
		return
	}

	var req createRunRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// Resolve dataset_id: either from request or by inlining.
	datasetID := req.DatasetID
	if req.DatasetInline != nil {
		if datasetID != "" {
			writeError(w, http.StatusBadRequest,
				"dataset_id and dataset_inline are mutually exclusive")
			return
		}
		// Check inline size limit (1 MiB).
		raw, err := json.Marshal(req.DatasetInline)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid dataset_inline: "+err.Error())
			return
		}
		if len(raw) > maxBodySize {
			writeError(w, http.StatusRequestEntityTooLarge,
				"dataset_inline exceeds 1 MiB; use POST /api/v1/datasets/{id}/upload instead")
			return
		}

		// Create a synthetic dataset with 1-hour TTL.
		ds := &dataset.Dataset{
			Name:   fmt.Sprintf("inline-%s", id.New()),
			Source: "inline",
			TTL:    time.Hour,
		}
		if err := s.deps.Datasets.Register(ds); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to register inline dataset: "+err.Error())
			return
		}
		// Ingest inline pools. DatasetInline is map[string]any where values are pool row arrays.
		for poolName, rows := range req.DatasetInline {
			rowSlice, ok := toRowSlice(rows)
			if !ok {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("dataset_inline.%s must be an array of objects", poolName))
				return
			}
			if err := s.deps.Datasets.AddRows(ds.ID, poolName, rowSlice); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		datasetID = ds.ID
	}

	// Validate dataset against workflow schema if both are present.
	if wf.DataSchema != nil && datasetID != "" {
		ds, ok := s.deps.Datasets.Get(datasetID)
		if !ok {
			writeError(w, http.StatusNotFound, "dataset not found: "+datasetID)
			return
		}
		result := dataset.ValidateDataset(ds, wf.DataSchema)
		if !result.OK {
			// Create run in rejected state.
			rn := &run.Run{
				ID:            req.RunID,
				WorkflowID:    workflowID,
				WorkflowName:  wf.Name,
				ExperimentID:  req.ExperimentID,
				DatasetID:     datasetID,
				Status:        run.StatusRejected,
				Labels:        req.Labels,
				WorkflowLabel: resolveWorkflowLabel(req.WorkflowLabel, wf.Name),
				Reason:        fmt.Sprintf("data validation failed: %v", result.Missing),
			}
			if err := s.deps.Runs.Create(rn); err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"run_id": rn.ID,
				"status": rn.Status,
				"reason": rn.Reason,
			})
			return
		}
	}

	// Generate meta_trace_id if not provided.
	metaTraceID := req.MetaTraceID
	if metaTraceID == "" {
		metaTraceID = id.NewLong()
	}

	rn := &run.Run{
		ID:            req.RunID,
		WorkflowID:    workflowID,
		WorkflowName:  wf.Name,
		ExperimentID:  req.ExperimentID,
		DatasetID:     datasetID,
		Status:        run.StatusStarting,
		MetaTraceID:   metaTraceID,
		WorkflowLabel: resolveWorkflowLabel(req.WorkflowLabel, wf.Name),
		Labels:        req.Labels,
	}
	if err := s.deps.Runs.Create(rn); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	// Set K6JobName after Create so rn.ID is guaranteed assigned.
	rn.K6JobName = fmt.Sprintf("zeus-run-%s", rn.ID)

	// Increment prometheus counter.
	s.deps.Metrics.RunsStartedTotal.WithLabelValues(workflowID, wf.Name).Inc()
	s.deps.Metrics.ActiveRuns.WithLabelValues().Inc()

	writeJSON(w, http.StatusAccepted, createRunResponse{
		RunID:       rn.ID,
		Status:      rn.Status,
		MetaTraceID: rn.MetaTraceID,
		K6JobName:   rn.K6JobName,
		StartedAt:   rn.StartedAt,
	})
}

// --- GET /api/v1/workflows/{id}/runs ---

func (s *Server) handleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("id")
	if _, ok := s.deps.Workflows.Get(workflowID); !ok {
		writeError(w, http.StatusNotFound, "workflow not found: "+workflowID)
		return
	}
	runs := s.deps.Runs.List(run.ListFilters{WorkflowID: workflowID})
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// --- GET /api/v1/runs ---

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filters := run.ListFilters{
		Status:       q.Get("status"),
		ExperimentID: q.Get("experiment_id"),
		WorkflowID:   q.Get("workflow_id"),
	}
	if since := q.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err == nil {
			filters.Since = t
		}
	}
	runs := s.deps.Runs.List(filters)
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// --- GET /api/v1/runs/{run_id} ---

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	rn, ok := s.deps.Runs.Get(runID)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found: "+runID)
		return
	}
	writeJSON(w, http.StatusOK, rn)
}

// --- DELETE /api/v1/runs/{run_id} ---

func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	rn, ok := s.deps.Runs.Get(runID)
	if !ok {
		writeError(w, http.StatusNotFound, "run not found: "+runID)
		return
	}
	if run.IsTerminal(rn.Status) {
		writeError(w, http.StatusConflict, "run is already in terminal state: "+rn.Status)
		return
	}
	if err := s.deps.Runs.UpdateStatus(runID, run.StatusStopped); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Prune prometheus run_id labels and decrement active runs gauge.
	s.deps.Metrics.PruneRunLabels(runID)
	s.deps.Metrics.ActiveRuns.WithLabelValues().Dec()
	s.deps.Metrics.RunsCompletedTotal.WithLabelValues(rn.WorkflowID, rn.WorkflowName, run.StatusStopped).Inc()

	// Close SSE subscribers.
	s.deps.Broker.CloseRun(runID)

	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/runs/{run_id}/events ---

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if _, ok := s.deps.Runs.Get(runID); !ok {
		writeError(w, http.StatusNotFound, "run not found: "+runID)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.deps.Broker.Subscribe(runID)
	defer unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-ch:
			if !open {
				return
			}
			writeSSEEvent(w, event)
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, event sse.Event) {
	if event.Type != "" {
		fmt.Fprintf(w, "event: %s\n", event.Type)
	}
	fmt.Fprintf(w, "data: %s\n\n", event.Data)
}

// --- GET /api/v1/runs/{run_id}/stats ---

func (s *Server) handleRunStats(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if _, ok := s.deps.Runs.Get(runID); !ok {
		writeError(w, http.StatusNotFound, "run not found: "+runID)
		return
	}
	snap, ok := s.deps.Snapshots.Get(runID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"run_id": runID,
			"status": "no stats available yet",
		})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// --- helpers ---

func resolveWorkflowLabel(explicit, workflowName string) string {
	if explicit != "" {
		return explicit
	}
	return workflowName
}

func runListFilters(status, experimentID, workflowID string) run.ListFilters {
	return run.ListFilters{
		Status:       status,
		ExperimentID: experimentID,
		WorkflowID:   workflowID,
	}
}

// toRowSlice converts an interface{} to []map[string]any (for inline dataset pools).
func toRowSlice(v any) ([]map[string]any, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	rows := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		rows = append(rows, row)
	}
	return rows, true
}
