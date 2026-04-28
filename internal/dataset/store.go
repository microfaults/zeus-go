package dataset

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"
	"time"

	gocache "github.com/patrickmn/go-cache"

	"atropos-go/loadgen/internal/id"
)

const (
	defaultTTL      = 24 * time.Hour
	cleanupInterval = 5 * time.Minute
)

// Store is a thread-safe in-memory store for datasets.
// TTL-based expiry is handled by go-cache; a domain mutex protects
// mutation operations like AddRows that touch pool state.
type Store struct {
	mu    sync.RWMutex // guards AddRows / Sample against concurrent mutation
	cache *gocache.Cache
}

// NewStore creates an empty dataset store with automatic TTL cleanup.
func NewStore() *Store {
	return &Store{
		cache: gocache.New(defaultTTL, cleanupInterval),
	}
}

// Register adds a dataset to the store. If d.ID is empty a random hex ID is
// assigned. Returns an error if a dataset with the same ID already exists.
func (s *Store) Register(d *Dataset) error {
	if d.ID == "" {
		d.ID = id.New()
	}
	if d.Pools == nil {
		d.Pools = make(map[string]*Pool)
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}

	ttl := d.TTL
	if ttl <= 0 {
		ttl = gocache.NoExpiration
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.cache.Get(d.ID); found {
		return fmt.Errorf("dataset: id %q already registered", d.ID)
	}
	s.cache.Set(d.ID, d, ttl)
	return nil
}

// Get returns a dataset by ID. Returns false if not found or expired.
func (s *Store) Get(dsID string) (*Dataset, bool) {
	v, found := s.cache.Get(dsID)
	if !found {
		return nil, false
	}
	return v.(*Dataset), true
}

// List returns all stored (non-expired) datasets.
func (s *Store) List() []*Dataset {
	items := s.cache.Items()
	out := make([]*Dataset, 0, len(items))
	for _, item := range items {
		if d, ok := item.Object.(*Dataset); ok {
			out = append(out, d)
		}
	}
	return out
}

// Delete removes a dataset by ID.
func (s *Store) Delete(dsID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, found := s.cache.Get(dsID); !found {
		return fmt.Errorf("dataset: id %q not found", dsID)
	}
	s.cache.Delete(dsID)
	return nil
}

// AddRows appends rows to the named pool within a dataset. If the pool does
// not exist it is created with fields inferred from the first row.
func (s *Store) AddRows(dsID string, poolName string, rows []map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, found := s.cache.Get(dsID)
	if !found {
		return fmt.Errorf("dataset: id %q not found", dsID)
	}
	d := v.(*Dataset)

	pool, exists := d.Pools[poolName]
	if !exists {
		pool = &Pool{}
		if len(rows) > 0 {
			fields := make([]string, 0, len(rows[0]))
			for k := range rows[0] {
				fields = append(fields, k)
			}
			pool.Fields = fields
		}
		d.Pools[poolName] = pool
	}

	pool.Rows = append(pool.Rows, rows...)
	pool.Stats.RowCount = len(pool.Rows)

	// Estimate size by encoding the new rows to JSON.
	for _, row := range rows {
		b, _ := json.Marshal(row)
		n := int64(len(b))
		pool.Stats.SizeBytes += n
		d.SizeBytes += n
	}
	return nil
}

// Sample returns up to limit random rows from the named pool.
func (s *Store) Sample(dsID string, poolName string, limit int) ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	v, found := s.cache.Get(dsID)
	if !found {
		return nil, fmt.Errorf("dataset: id %q not found", dsID)
	}
	d := v.(*Dataset)

	pool, ok := d.Pools[poolName]
	if !ok {
		return nil, fmt.Errorf("dataset: pool %q not found in dataset %q", poolName, dsID)
	}

	n := len(pool.Rows)
	if n == 0 {
		return nil, nil
	}
	if limit >= n {
		out := make([]map[string]any, n)
		copy(out, pool.Rows)
		return out, nil
	}

	// Fisher-Yates partial shuffle using crypto/rand.
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	for i := 0; i < limit; i++ {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(n-i)))
		idx := i + int(j.Int64())
		indices[i], indices[idx] = indices[idx], indices[i]
	}
	out := make([]map[string]any, limit)
	for i := 0; i < limit; i++ {
		out[i] = pool.Rows[indices[i]]
	}
	return out, nil
}

// Count returns the number of datasets in the store.
func (s *Store) Count() int {
	return s.cache.ItemCount()
}
