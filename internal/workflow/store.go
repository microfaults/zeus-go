package workflow

import (
	"fmt"
	"sync"

	"atropos-go/loadgen/internal/id"
)

// Store is a thread-safe in-memory store for workflows.
type Store struct {
	mu        sync.RWMutex
	workflows map[string]*Workflow
	nameIndex map[string]string // name → id
}

// NewStore creates an empty workflow store.
func NewStore() *Store {
	return &Store{
		workflows: make(map[string]*Workflow),
		nameIndex: make(map[string]string),
	}
}

// Register adds a workflow to the store.
// Assigns an ID if empty, enforces name uniqueness, and stores the workflow.
func (s *Store) Register(w *Workflow) error {
	if w.ID == "" {
		w.ID = id.New()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.workflows[w.ID]; exists {
		return fmt.Errorf("workflow: id %q already exists (409)", w.ID)
	}
	if existingID, exists := s.nameIndex[w.Name]; exists && existingID != w.ID {
		return fmt.Errorf("workflow: name %q already registered under id %q (409)", w.Name, existingID)
	}

	s.workflows[w.ID] = w
	s.nameIndex[w.Name] = w.ID
	return nil
}

// Get returns a workflow by ID.
func (s *Store) Get(id string) (*Workflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.workflows[id]
	return w, ok
}

// GetByName returns a workflow by its unique name.
func (s *Store) GetByName(name string) (*Workflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.nameIndex[name]
	if !ok {
		return nil, false
	}
	w, ok := s.workflows[id]
	return w, ok
}

// List returns all stored workflows.
func (s *Store) List() []*Workflow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Workflow, 0, len(s.workflows))
	for _, w := range s.workflows {
		out = append(out, w)
	}
	return out
}

// Delete removes a workflow by ID.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, exists := s.workflows[id]
	if !exists {
		return fmt.Errorf("workflow: id %q not found", id)
	}
	delete(s.nameIndex, w.Name)
	delete(s.workflows, id)
	return nil
}
