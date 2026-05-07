package stats

import (
	"sync"
	"time"

	"atropos-go/loadgen/internal/attacker"
)

// RunStats holds finalized stats for a completed workflow run.
type RunStats struct {
	RunID               string           `json:"run_id"`
	WorkflowID          string           `json:"workflow_id"`
	Status              string           `json:"status"`
	Duration            time.Duration    `json:"duration"`
	Iterations          int64            `json:"iterations"`
	RequestsSent        int64            `json:"requests_sent"`
	RequestsOK          int64            `json:"requests_ok"`
	RequestsDropped     int64            `json:"requests_dropped"`
	LatencyP50          time.Duration    `json:"latency_p50"`
	LatencyP95          time.Duration    `json:"latency_p95"`
	LatencyP99          time.Duration    `json:"latency_p99"`
	VariantDistribution map[string]int64 `json:"variant_distribution"`
	FinalizedAt         time.Time        `json:"finalized_at"`
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
