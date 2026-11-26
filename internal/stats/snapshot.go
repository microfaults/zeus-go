package stats

import (
	"sync"
	"time"

	"atropos-go/loadgen/internal/attacker"
)

// Snapshot sources (RunStats.Source).
const (
	// SourceSummary marks numbers taken from k6's --summary-export, the
	// authoritative end-of-run aggregate of a run that k6 finished.
	SourceSummary = "k6_summary"
	// SourceStream marks numbers zeus aggregated itself from k6's live JSON
	// sample output: every interim snapshot, and the final snapshot of a
	// run that ended without a summary export (stopped, failed). Stream
	// latency percentiles are approximate (within ~1% relative error).
	SourceStream = "k6_stream"
)

// RunStats is a run's load-side stats snapshot: interim while the run is in
// flight, final once it reached a terminal state.
type RunStats struct {
	RunID      string `json:"run_id"`
	WorkflowID string `json:"workflow_id"`
	// Status is the run's status when the snapshot was taken.
	Status string `json:"status"`
	// Final is false for an interim snapshot (refreshed every few seconds
	// while the run is in flight) and true once the run is terminal and the
	// snapshot will not change again.
	Final bool `json:"final"`
	// Source is SourceSummary or SourceStream.
	Source string `json:"source"`
	// Duration is the run's elapsed time so far, in nanoseconds.
	Duration        time.Duration `json:"duration"`
	Iterations      int64         `json:"iterations"`
	RequestsSent    int64         `json:"requests_sent"`
	RequestsOK      int64         `json:"requests_ok"`
	RequestsDropped int64         `json:"requests_dropped"`
	// ActiveVUs is k6's last reported VU count (interim snapshots only;
	// 0 in final snapshots).
	ActiveVUs  int64         `json:"active_vus"`
	LatencyP50 time.Duration `json:"latency_p50"`
	LatencyP95 time.Duration `json:"latency_p95"`
	LatencyP99 time.Duration `json:"latency_p99"`
	// VariantDistribution is reserved: the k6 engine does not report variant
	// picks, so it is always null.
	VariantDistribution map[string]int64 `json:"variant_distribution"`
	// UpdatedAt is when this snapshot was taken.
	UpdatedAt time.Time `json:"updated_at"`
	// FinalizedAt is set on final snapshots only.
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// AttackStats holds finalized stats for a completed attack, embedding the
// base AttackResult with additional dedup metrics.
type AttackStats struct {
	attacker.AttackResult
	DedupVariants   int64 `json:"dedup_variants"`
	DedupCollisions int64 `json:"dedup_collisions"`
}

// SnapshotStore is a thread-safe map of run_id to RunStats.
type SnapshotStore struct {
	mu    sync.RWMutex
	store map[string]*RunStats
}

// NewSnapshotStore creates a new SnapshotStore.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		store: make(map[string]*RunStats),
	}
}

// Save stores a RunStats snapshot by its RunID.
func (s *SnapshotStore) Save(stats *RunStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[stats.RunID] = stats
}

// Get retrieves a RunStats snapshot by run ID.
func (s *SnapshotStore) Get(runID string) (*RunStats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.store[runID]
	return st, ok
}

// Delete removes a RunStats snapshot by run ID.
func (s *SnapshotStore) Delete(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.store, runID)
}
