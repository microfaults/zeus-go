package dataset

import "time"

// Dataset represents a named collection of data pools used by load test workflows.
type Dataset struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Source    string           `json:"source"` // "upload", "cache_box_dump", "inline"
	Pools     map[string]*Pool `json:"pools"`
	TTL       time.Duration    `json:"ttl"`
	SizeBytes int64            `json:"size_bytes"`
	CreatedAt time.Time        `json:"created_at"`
}

// Pool is a named table of rows sharing a common field set.
type Pool struct {
	Fields []string         `json:"fields"`
	Rows   []map[string]any `json:"rows"`
	Stats  PoolStats        `json:"stats"`
}

// PoolStats tracks aggregate metrics for a pool.
type PoolStats struct {
	RowCount  int   `json:"row_count"`
	SizeBytes int64 `json:"size_bytes"`
}
