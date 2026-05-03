package run

import (
	"fmt"
	"sync"
	"time"

	"atropos-go/loadgen/internal/id"
)

// ListFilters controls which runs are returned by List.
type ListFilters struct {
	Status       string
	ExperimentID string
	WorkflowID   string
	Since        time.Time
}

// Store is a thread-safe in-memory store for runs.
type Store struct {
	mu   sync.RWMutex
	runs map[string]*Run
}

// NewStore creates an empty run store.
func NewStore() *Store {
	return &Store{
		runs: make(map[string]*Run),
	}
}

// Create adds a new run to the store.
// Assigns an ID if empty, validates required fields, sets StartedAt, and stores the run.
func (s *Store) Create(r *Run) error {
	if r.WorkflowID == "" {
		return fmt.Errorf("run: workflow_id is required")
	}
	if r.ID == "" {
		r.ID = id.New()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.runs[r.ID]; exists {
		return fmt.Errorf("run: id %q already exists (409)", r.ID)
	}

	r.StartedAt = time.Now()
	s.runs[r.ID] = r
	return nil
}

// Get returns a run by ID.
func (s *Store) Get(id string) (*Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	return r, ok
}

// List returns runs matching the given filters.
func (s *Store) List(filters ListFilters) []*Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Run, 0, len(s.runs))
	for _, r := range s.runs {
		if filters.Status != "" && r.Status != filters.Status {
			continue
		}
		if filters.ExperimentID != "" && r.ExperimentID != filters.ExperimentID {
			continue
		}
		if filters.WorkflowID != "" && r.WorkflowID != filters.WorkflowID {
			continue
		}
		if !filters.Since.IsZero() && r.StartedAt.Before(filters.Since) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// UpdateStatus transitions a run to a new status.
// Validates the transition and sets EndedAt on terminal states.
func (s *Store) UpdateStatus(id string, newStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run: id %q not found", id)
	}
	if err := Transition(r.Status, newStatus); err != nil {
		return err
	}
	r.Status = newStatus
	if IsTerminal(newStatus) {
		now := time.Now()
		r.EndedAt = &now
	}
	return nil
}

// UpdateReason sets the reason field on a run.
func (s *Store) UpdateReason(id string, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("run: id %q not found", id)
	}
	r.Reason = reason
	return nil
}

// Delete removes a run by ID.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[id]; !exists {
		return fmt.Errorf("run: id %q not found", id)
	}
	delete(s.runs, id)
	return nil
}

// CountByStatus returns the number of runs with the given status.
func (s *Store) CountByStatus(status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, r := range s.runs {
		if r.Status == status {
			count++
		}
	}
	return count
}
