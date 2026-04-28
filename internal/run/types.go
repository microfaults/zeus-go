package run

import "time"

// Run status constants.
const (
	StatusStarting   = "starting"
	StatusValidating = "validating"
	StatusRunning    = "running"
	StatusCompleting = "completing"
	StatusCompleted  = "completed"
	StatusStopped    = "stopped"
	StatusFailed     = "failed"
	StatusRejected   = "rejected"
)

// Run represents a single execution of a workflow.
type Run struct {
	ID            string            `json:"id"`
	WorkflowID    string            `json:"workflow_id"`
	WorkflowName  string            `json:"workflow_name"`
	ExperimentID  string            `json:"experiment_id"`
	DatasetID     string            `json:"dataset_id"`
	Status        string            `json:"status"`
	MetaTraceID   string            `json:"meta_trace_id"`
	WorkflowLabel string            `json:"workflow_label"`
	Labels        map[string]string `json:"labels"`
	K6JobName     string            `json:"k6_job_name"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       *time.Time        `json:"ended_at,omitempty"`
	Reason        string            `json:"reason,omitempty"`
}
