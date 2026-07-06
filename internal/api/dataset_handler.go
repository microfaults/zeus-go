package api

import (
	"net/http"
	"strconv"
	"time"

	"atropos-go/loadgen/internal/dataset"
	"atropos-go/loadgen/internal/run"
)

// --- POST /api/v1/datasets ---

type createDatasetRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	TTLS   int    `json:"ttl_s"`
}

// handleCreateDataset registers a new dataset shell.
//
// @Summary      Register dataset
// @Description  Persist a dataset shell. Source is one of "upload" (NDJSON push), "inline"
// @Description  (synthesized by run-create from dataset_inline), or "cache_box_dump" (future).
// @Description  Defaults: source="upload", ttl_s=86400 (24 h). Rows are pushed separately via
// @Description  POST /datasets/{id}/upload.
// @Tags         datasets
// @Accept       json
// @Produce      json
// @Param        body  body      api.createDatasetRequest  true  "dataset shell"
// @Success      201   {object}  map[string]any            "envelope: id, name, source, created_at, ttl_s"
// @Failure      400   {object}  api.ErrorResponse         "invalid JSON"
// @Failure      409   {object}  api.ErrorResponse         "dataset name conflict"
// @Router       /datasets [post]
func (s *Server) handleCreateDataset(w http.ResponseWriter, r *http.Request) {
	var req createDatasetRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	ttl := time.Duration(req.TTLS) * time.Second
	if req.TTLS == 0 {
		ttl = 24 * time.Hour // default 24h
	}

	source := req.Source
	if source == "" {
		source = "upload"
	}

	ds := &dataset.Dataset{
		Name:   req.Name,
		Source: source,
		TTL:    ttl,
	}
	if err := s.deps.Datasets.Register(ds); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         ds.ID,
		"name":       ds.Name,
		"source":     ds.Source,
		"created_at": ds.CreatedAt,
		"ttl_s":      int(ds.TTL.Seconds()),
	})
}

// --- POST /api/v1/datasets/{id}/upload ---

// handleUploadDataset streams NDJSON pool batches into an existing dataset.
//
// @Summary      Upload dataset rows (NDJSON)
// @Description  Streams NDJSON pool batches into the dataset. Each line is
// @Description  {"pool":"<name>","rows":[{...},...]}. The endpoint is lenient: per-line decode
// @Description  errors are collected and returned in the response body but do not abort the
// @Description  ingest. The response envelope contains ingested (per-pool counts), total
// @Description  (rows accepted), and an optional errors array.
// @Tags         datasets
// @Accept       application/x-ndjson
// @Produce      json
// @Param        id    path      string  true  "Dataset ID"
// @Success      200   {object}  map[string]any     "ingest result envelope"
// @Failure      400   {object}  api.ErrorResponse  "ingest error (e.g. dataset not found at ingest time)"
// @Failure      404   {object}  api.ErrorResponse  "dataset not found"
// @Router       /datasets/{id}/upload [post]
func (s *Server) handleUploadDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.deps.Datasets.Get(id); !ok {
		writeError(w, http.StatusNotFound, "dataset not found: "+id)
		return
	}

	result, err := dataset.IngestNDJSON(s.deps.Datasets, id, r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := map[string]any{
		"ingested": result.PoolCounts,
		"total":    result.TotalRows,
	}
	if len(result.Errors) > 0 {
		resp["errors"] = result.Errors
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /api/v1/datasets ---

// handleListDatasets returns a wrapper containing dataset summaries.
//
// @Summary      List datasets
// @Description  Returns an envelope { "datasets": [...] } with summary fields per dataset
// @Description  (id, name, source, size_bytes, ttl_s, created_at). Use GET /datasets/{id}
// @Description  for full pool stats.
// @Tags         datasets
// @Produce      json
// @Success      200  {object}  map[string]any  "envelope with datasets array"
// @Router       /datasets [get]
func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	list := s.deps.Datasets.List()
	type item struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Source    string    `json:"source"`
		SizeBytes int64     `json:"size_bytes"`
		TTLS      int       `json:"ttl_s"`
		CreatedAt time.Time `json:"created_at"`
	}
	items := make([]item, len(list))
	for i, ds := range list {
		items[i] = item{
			ID:        ds.ID,
			Name:      ds.Name,
			Source:    ds.Source,
			SizeBytes: ds.SizeBytes,
			TTLS:      int(ds.TTL.Seconds()),
			CreatedAt: ds.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": items})
}

// --- GET /api/v1/datasets/{id} ---

// handleGetDataset returns a dataset's metadata and pool stats.
//
// @Summary      Get dataset
// @Description  Returns an envelope with id, name, source, pool_stats (per-pool row_count,
// @Description  size_bytes, fields), size_bytes, ttl_s, created_at. Use GET /datasets/{id}/sample
// @Description  to inspect rows.
// @Tags         datasets
// @Produce      json
// @Param        id   path      string  true  "Dataset ID"
// @Success      200  {object}  map[string]any  "dataset metadata envelope"
// @Failure      404  {object}  api.ErrorResponse  "dataset not found"
// @Router       /datasets/{id} [get]
func (s *Server) handleGetDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ds, ok := s.deps.Datasets.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "dataset not found: "+id)
		return
	}

	// Build pool_stats from the dataset's pools.
	poolStats := make(map[string]any, len(ds.Pools))
	for name, pool := range ds.Pools {
		poolStats[name] = map[string]any{
			"row_count":  pool.Stats.RowCount,
			"size_bytes": pool.Stats.SizeBytes,
			"fields":     pool.Fields,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         ds.ID,
		"name":       ds.Name,
		"source":     ds.Source,
		"pool_stats": poolStats,
		"size_bytes": ds.SizeBytes,
		"ttl_s":      int(ds.TTL.Seconds()),
		"created_at": ds.CreatedAt,
	})
}

// --- GET /api/v1/datasets/{id}/sample ---

// handleSampleDataset returns up to N rows from a dataset's named pool.
//
// @Summary      Sample dataset rows
// @Description  Returns up to limit rows (default 10, capped at 100) from the named pool.
// @Description  The response envelope is { "pool": "<name>", "rows": [...] }.
// @Tags         datasets
// @Produce      json
// @Param        id     path      string  true   "Dataset ID"
// @Param        pool   query     string  true   "Pool name to sample"
// @Param        limit  query     int     false  "Max rows to return (default 10, max 100)"
// @Success      200    {object}  map[string]any  "sample envelope"
// @Failure      400    {object}  api.ErrorResponse  "missing pool query parameter"
// @Failure      404    {object}  api.ErrorResponse  "dataset or pool not found"
// @Router       /datasets/{id}/sample [get]
func (s *Server) handleSampleDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		writeError(w, http.StatusBadRequest, "pool query parameter is required")
		return
	}

	limit := 10
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}

	rows, err := s.deps.Datasets.Sample(id, pool, limit)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"pool": pool,
		"rows": rows,
	})
}

// --- DELETE /api/v1/datasets/{id} ---

// handleDeleteDataset deletes a dataset if no active run references it.
//
// @Summary      Delete dataset
// @Description  Refuses with 409 when an active run still references the dataset.
// @Tags         datasets
// @Produce      json
// @Param        id   path      string  true  "Dataset ID"
// @Success      204  "dataset deleted"
// @Failure      404  {object}  api.ErrorResponse  "dataset not found"
// @Failure      409  {object}  api.ErrorResponse  "dataset is in use by an active run"
// @Router       /datasets/{id} [delete]
func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Check if any active run references this dataset.
	runs := s.deps.Runs.List(runListFilters("", "", ""))
	for _, rn := range runs {
		if rn.DatasetID == id && !run.IsTerminal(rn.Status) {
			writeError(w, http.StatusConflict, "dataset is in use by active run: "+rn.ID)
			return
		}
	}

	if err := s.deps.Datasets.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDatasetContent returns the full dataset as the flat pool map the k6
// engine consumes: {"<pool>": [ {row}, ... ], ...}. This is the endpoint the
// launcher passes as ZEUS_DATASET_ENDPOINT (engine setup() JSON.parses the
// body and indexes it as data.<pool>). Distinct from /sample (one pool,
// capped) and /{id} (metadata only) -- neither has the shape the engine
// wants. No row cap: a run needs the whole pool, and datasets are already
// size-bounded at ingest.
//
// @Summary      Dataset content (engine shape)
// @Description  Returns the full { "<pool>": [rows...] } map the k6 engine consumes.
// @Tags         datasets
// @Produce      json
// @Param        id   path      string  true  "Dataset ID"
// @Success      200  {object}  map[string]any  "pool -> rows"
// @Failure      404  {object}  api.ErrorResponse  "dataset not found"
// @Router       /datasets/{id}/content [get]
func (s *Server) handleDatasetContent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ds, ok := s.deps.Datasets.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "dataset not found: "+id)
		return
	}
	out := make(map[string][]map[string]any, len(ds.Pools))
	for name, pool := range ds.Pools {
		out[name] = pool.Rows
	}
	writeJSON(w, http.StatusOK, out)
}
