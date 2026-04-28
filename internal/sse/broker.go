package sse

import "sync"

// Event represents an SSE event bound to a specific run.
type Event struct {
	Type  string `json:"type"`  // e.g. "step.ok", "step.drop", "iteration.done", "stats.snapshot", "run.state"
	Data  string `json:"data"`  // JSON payload
	RunID string `json:"run_id"`
}

// Broker fans out events to per-run subscriber channels.
type Broker struct {
	mu   sync.RWMutex
	subs map[string][]chan Event // run_id -> subscriber channels
}

// NewBroker creates an empty event broker.
func NewBroker() *Broker {
	return &Broker{
		subs: make(map[string][]chan Event),
	}
}

// Subscribe registers a new subscriber for the given runID. It returns a
// receive-only channel and an unsubscribe function that must be called when
// the subscriber is done.
func (b *Broker) Subscribe(runID string) (<-chan Event, func()) {
	ch := make(chan Event, 256)

	b.mu.Lock()
	b.subs[runID] = append(b.subs[runID], ch)
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		channels := b.subs[runID]
		for i, c := range channels {
			if c == ch {
				b.subs[runID] = append(channels[:i], channels[i+1:]...)
				break
			}
		}
		// Clean up the map entry if no subscribers remain.
		if len(b.subs[runID]) == 0 {
			delete(b.subs, runID)
		}
	}

	return ch, unsubscribe
}

// Publish sends an event to all subscribers for the given runID. If a
// subscriber channel is full the event is dropped (non-blocking send).
func (b *Broker) Publish(runID string, event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs[runID] {
		select {
		case ch <- event:
		default:
			// subscriber is slow; drop event
		}
	}
}

// CloseRun closes all subscriber channels for the run and removes the entry.
// This causes subscriber goroutines that range over the channel to exit.
func (b *Broker) CloseRun(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ch := range b.subs[runID] {
		close(ch)
	}
	delete(b.subs, runID)
}

// SubscriberCount returns the number of active subscribers for the given runID.
func (b *Broker) SubscriberCount(runID string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[runID])
}
