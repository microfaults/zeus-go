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
