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
	RateRPS       float64           `json:"rate_rps"`
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

// handleCreateRun starts a workflow run. Dataset binding is handshake-aware:
// rejection on schema validation produces a terminal "rejected" run with 422,
// not 400. See the run lifecycle state machine in docs/api-contract.md.
//
// @Summary      Start workflow run
// @Description  Starts a run for the given workflow. Either dataset_id or dataset_inline (capped
// @Description  at 1 MiB) may be supplied — they are mutually exclusive. When the workflow
// @Description  declares a DataSchema, the dataset is validated; failures produce a terminal
// @Description  "rejected" run and a 422 response with {run_id, status, reason}. Other run-level
// @Description  rejections (workflow not found, run_id conflict, oversized inline) return their
// @Description  natural codes; the run is not persisted in those cases. The success body is a
// @Description  202-Accepted envelope of run_id, status, meta_trace_id, k6_job_name, started_at.
// @Tags         runs
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Workflow ID"
// @Param        body  body      api.createRunRequest    true  "run request"
// @Success      202   {object}  api.createRunResponse
// @Failure      400   {object}  api.ErrorResponse  "invalid JSON, mutually exclusive dataset fields, or pool shape error"
// @Failure      404   {object}  api.ErrorResponse  "workflow or referenced dataset not found"
// @Failure      409   {object}  api.ErrorResponse  "run_id conflict"
// @Failure      413   {object}  api.ErrorResponse  "dataset_inline exceeds 1 MiB"
// @Failure      422   {object}  map[string]any     "dataset failed schema validation; run created in rejected state"
// @Failure      500   {object}  api.ErrorResponse  "inline dataset persistence failure"
// @Router       /workflows/{id}/runs [post]
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

	// Snapshot the response BEFORE Launch: the launcher advances rn.Status
	// from a supervision goroutine, so reading rn's fields after Launch
	// would race that write.
	resp := createRunResponse{
		RunID:       rn.ID,
		Status:      rn.Status,
		MetaTraceID: rn.MetaTraceID,
		K6JobName:   rn.K6JobName,
		StartedAt:   rn.StartedAt,
	}

	// Count the run before handing it over: the launcher decrements the
	// active gauge when it finalizes the run, which can happen before
	// Launch even returns.
	s.deps.Metrics.RunsStartedTotal.WithLabelValues(workflowID, wf.Name).Inc()
	s.deps.Metrics.ActiveRuns.WithLabelValues().Inc()

	// Hand the run to the k6 launcher: it owns every later status
	// transition (validating → running → completing → completed/failed).
	// A launch error already finalized the run as failed with the reason.
	if s.deps.Launcher != nil {
		if err := s.deps.Launcher.Launch(rn, wf.Document(), run.LaunchSpec{
			VUs:           req.VUs,
			RateRPS:       req.RateRPS,
			DurationS:     req.DurationS,
			Persona:       req.Persona,
			BaseURL:       wf.BaseURL,
			DatasetID:     datasetID,
			WorkflowLabel: rn.WorkflowLabel,
			MetaTraceID:   rn.MetaTraceID,
		}); err != nil {
			s.deps.Metrics.ActiveRuns.WithLabelValues().Dec()
			s.deps.Metrics.RunsCompletedTotal.WithLabelValues(workflowID, wf.Name, run.StatusFailed).Inc()
			writeError(w, http.StatusInternalServerError, "launch k6: "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusAccepted, resp)
}

// --- GET /api/v1/workflows/{id}/runs ---

// handleListWorkflowRuns returns runs scoped to a single workflow.
//
// @Summary      List runs for a workflow
// @Description  Returns an envelope { "runs": [...] } of all runs (including terminal) bound
// @Description  to the workflow.
// @Tags         runs
// @Produce      json
// @Param        id   path      string  true  "Workflow ID"
// @Success      200  {object}  map[string]any  "envelope with runs array"
// @Failure      404  {object}  api.ErrorResponse  "workflow not found"
// @Router       /workflows/{id}/runs [get]
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

// handleListRuns returns cross-workflow runs filtered by query parameters.
//
// @Summary      List runs (cross-workflow)
// @Description  Returns an envelope { "runs": [...] }. Filters are AND-combined; "since" must be
// @Description  RFC 3339 (invalid timestamps are silently ignored).
// @Tags         runs
// @Produce      json
// @Param        status         query     string  false  "filter by run status"
// @Param        experiment_id  query     string  false  "filter by experiment id"
// @Param        workflow_id    query     string  false  "filter by workflow id"
// @Param        since          query     string  false  "RFC 3339 lower bound on started_at"
// @Success      200  {object}  map[string]any  "envelope with runs array"
// @Router       /runs [get]
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

// handleGetRun returns a single run document.
//
// @Summary      Get run
// @Tags         runs
// @Produce      json
// @Param        run_id  path      string  true  "Run ID"
// @Success      200     {object}  run.Run
// @Failure      404     {object}  api.ErrorResponse  "run not found"
// @Router       /runs/{run_id} [get]
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

// handleStopRun stops a non-terminal run. The stats gathered up to the stop
// are kept: the launcher saves them as the run's final snapshot, publishes
// it on the event stream, and then closes the stream.
//
// @Summary      Stop run
// @Description  Transitions a non-terminal run to "stopped" and kills its k6 process. Refuses
// @Description  with 409 when the run is already in a terminal state
// @Description  (completed/stopped/failed/rejected). The stats gathered up to the stop are kept:
// @Description  GET /runs/{run_id}/stats then serves a final snapshot (status "stopped",
// @Description  source "k6_stream") built from k6's live output; k6 writes no summary export
// @Description  when killed. The run's Prometheus series keep their last values. The event
// @Description  stream closes after the final stats.snapshot event.
// @Tags         runs
// @Produce      json
// @Param        run_id  path      string  true  "Run ID"
// @Success      204     "run stopped"
// @Failure      404     {object}  api.ErrorResponse  "run not found"
// @Failure      409     {object}  api.ErrorResponse  "run is already in terminal state"
// @Router       /runs/{run_id} [delete]
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
	// Status first, then kill: the launcher's supervision goroutine sees the
	// context cancellation, leaves the terminal 'stopped' status alone,
	// saves the stats gathered so far and closes the event stream itself.
	killed := s.deps.Launcher != nil && s.deps.Launcher.Stop(runID)

	// Per-run Prometheus series are NOT pruned here: they keep their final
	// values until the run record is evicted (see run.Store.OnEvict).
	s.deps.Metrics.ActiveRuns.WithLabelValues().Dec()
	s.deps.Metrics.RunsCompletedTotal.WithLabelValues(rn.WorkflowID, rn.WorkflowName, run.StatusStopped).Inc()

	if !killed {
		// No live k6 process, so no supervisor will close the stream.
		s.deps.Broker.CloseRun(runID)
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- GET /api/v1/runs/{run_id}/events ---

// handleRunEvents streams run events as Server-Sent Events.
//
// @Summary      Stream run events (SSE)
// @Description  Server-Sent Events stream of a run's progress. Event types emitted:
// @Description  run.state (data {run_id, status, detail}) on every status change, and
// @Description  stats.snapshot (data: the same document as GET /runs/{run_id}/stats) at least
// @Description  every 5 seconds while the run is in flight plus a final one when it ends. On
// @Description  connect the stream first sends the run's current run.state (and its latest
// @Description  stats.snapshot, if any). A comment heartbeat (": heartbeat") goes out at least
// @Description  every 15 seconds. The stream stays open for the whole run, whatever its length,
// @Description  and closes once the run is terminal (immediately, for a run that already is).
// @Description  Events carry no id; the Last-Event-ID header is not honored (a subscriber starts
// @Description  at the live edge). Used by manteion for live dashboards; poll
// @Description  GET /runs/{run_id}/stats for the authoritative final numbers.
// @Tags         runs
// @Produce      text/event-stream
// @Param        run_id        path    string  true   "Run ID"
// @Success      200  "SSE stream of run events"
// @Failure      404  {object}  api.ErrorResponse  "run not found"
// @Failure      500  {object}  api.ErrorResponse  "streaming not supported by the underlying response writer"
// @Router       /runs/{run_id}/events [get]
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

	// The server's read/write timeouts would cut the stream mid-run (the
	// write deadline is absolute from the request start). Lift them for
	// this response only; every other endpoint keeps them. Recorders and
	// other writers that cannot do this just keep the server defaults.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE reading the run's state: a run turning terminal after
	// the read closes this subscription, and one that turned terminal
	// before it is caught by the read.
	ch, unsubscribe := s.deps.Broker.Subscribe(runID)
	defer unsubscribe()

	rn, ok := s.deps.Runs.Get(runID)
	if !ok {
		return
	}
	writeSSEEvent(w, runStateEvent(rn))
	if snap, ok := s.deps.Snapshots.Get(runID); ok {
		if payload, err := json.Marshal(snap); err == nil {
			writeSSEEvent(w, sse.Event{Type: "stats.snapshot", Data: string(payload), RunID: runID})
		}
	}
	flusher.Flush()
	if run.IsTerminal(rn.Status) {
		return
	}

	heartbeat := time.NewTicker(s.deps.SSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case event, open := <-ch:
			if !open {
				return
			}
			writeSSEEvent(w, event)
			flusher.Flush()
		}
	}
}

// runStateEvent renders a run's current status as a run.state event (the
// same payload the launcher publishes on transitions).
func runStateEvent(rn *run.Run) sse.Event {
	payload, _ := json.Marshal(map[string]string{
		"run_id": rn.ID, "status": rn.Status, "detail": rn.Reason,
	})
	return sse.Event{Type: "run.state", Data: string(payload), RunID: rn.ID}
}

func writeSSEEvent(w http.ResponseWriter, event sse.Event) {
	if event.Type != "" {
		fmt.Fprintf(w, "event: %s\n", event.Type)
	}
	fmt.Fprintf(w, "data: %s\n\n", event.Data)
}

// --- GET /api/v1/runs/{run_id}/stats ---

// handleRunStats returns the run's current stats snapshot: interim while the
// run is in flight, final once it is terminal.
//
// @Summary      Get run stats
// @Description  Returns the run's RunStats snapshot. While the run is in flight the snapshot is
// @Description  interim ("final": false, "source": "k6_stream"): counts and latency percentiles
// @Description  so far, aggregated by zeus from k6's live output and refreshed at least every
// @Description  5 seconds (stream percentiles are approximate, within ~1%). Once the run is
// @Description  terminal the snapshot is final ("final": true): for a run k6 finished
// @Description  ("completed") the numbers come from k6's summary export ("source": "k6_summary");
// @Description  for a stopped or failed run they are the stream totals gathered up to the end
// @Description  ("source": "k6_stream"). The final snapshot can trail the terminal status by
// @Description  a moment, so poll until "final" is true. Before any snapshot exists (the run
// @Description  has not started k6, or never did) the response is 200 with a minimal envelope
// @Description  { "run_id": ..., "status": "no stats available yet" }. Tagged "metrics" rather
// @Description  than "runs" because it serves aggregate numbers, not run-document state.
// @Tags         metrics
// @Produce      json
// @Param        run_id  path      string  true  "Run ID"
// @Success      200     {object}  stats.RunStats  "stats snapshot (or pending placeholder envelope)"
// @Failure      404     {object}  api.ErrorResponse  "run not found"
// @Router       /runs/{run_id}/stats [get]
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
