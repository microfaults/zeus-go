package workflow

import (
	"encoding/json"
	"time"
)

// Workflow represents a Zeus DSL v2 workflow document.
type Workflow struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	Version           string              `json:"version"`
	Description       string              `json:"description"`
	Targets           []string            `json:"targets"`
	BaseURL           string              `json:"base_url"`
	EstimatedRPSPerVU int                 `json:"estimated_rps_per_vu"`
	Thresholds        map[string][]string `json:"thresholds"`
	DefaultDelay      *DelaySpec          `json:"default_delay,omitempty"`
	DataSchema        *DataSchema         `json:"data_schema,omitempty"`
	Root              json.RawMessage     `json:"root"`
	CreatedAt         time.Time           `json:"created_at"`
}

// DataSchema describes the data pools a workflow expects.
type DataSchema struct {
	Pools map[string]PoolSchema `json:"pools"`
}

// PoolSchema describes a single data pool.
type PoolSchema struct {
	Fields   []string `json:"fields"`
	MinSize  int      `json:"min_size"`
	Optional bool     `json:"optional"`
}

// DelaySpec represents a random delay range in milliseconds.
type DelaySpec struct {
	MinMs int `json:"min_ms"`
	MaxMs int `json:"max_ms"`
}
