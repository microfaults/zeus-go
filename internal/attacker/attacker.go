package attacker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"

	"atropos-go/loadgen/internal/dedup"
	"atropos-go/loadgen/internal/trace"
)

// Dedup bypass strategy identifiers and defaults.
const (
	DedupStrategyHeader = "header"
	DedupStrategyQuery  = "query"

	DefaultHeaderDedupName = "X-Idempotency-Key"
	DefaultQueryDedupName  = "nonce"
)

// TargetSpec defines the HTTP target for an attack.
type TargetSpec struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// DedupBypassSpec selects a strategy for defeating idempotency dedup on the
// target service. Each strategy mutates a different request field per request:
//
//	header → randomizes a header value (Source = header name)
//	query  → randomizes a query parameter (Source = param name)
//
// When Source is empty, a strategy-specific default is used (see
// DefaultHeaderDedupName, DefaultQueryDedupName).
type DedupBypassSpec struct {
	Strategy string `json:"strategy"`
	Source   string `json:"source,omitempty"`
}

// AttackConfig defines a single targeted attack.
//
// Cardinality note: ExperimentID is used as a Prometheus label on the
// AttacksStartedTotal counter. The counter is never pruned, so callers are
// responsible for keeping experiment id cardinality bounded (manteion is the
// canonical source). Zeus does NOT validate WorkflowLabel against any atropos
// rule registry — a label that no atropos rule matches will produce a
// no-isolation run silently. Manteion owns that cross-check.
type AttackConfig struct {
	ID        string     `json:"id"`
	Target    TargetSpec `json:"target"`
	Rate      int        `json:"rate"`
	DurationS int        `json:"duration_s"`

	// Optional vegeta tuning. Omit (zero) to use vegeta defaults.
	TimeoutS       int   `json:"timeout_s,omitempty"`
	MaxConnections int   `json:"max_connections,omitempty"`
	MaxBodyBytes   int64 `json:"max_body_bytes,omitempty"`
	Redirects      int   `json:"redirects,omitempty"`

	DedupBypass   *DedupBypassSpec `json:"dedup_bypass,omitempty"`
	MetaTraceID   string           `json:"meta_trace_id,omitempty"`
	ExperimentID  string           `json:"experiment_id,omitempty"`
	RunRef        string           `json:"run_ref,omitempty"`
	WorkflowLabel string           `json:"workflow_label,omitempty"`
}

// Duration returns the configured attack duration as a time.Duration.
func (c *AttackConfig) Duration() time.Duration {
	return time.Duration(c.DurationS) * time.Second
}

// Validate checks the attack config before execution and fills in defaults
// (HTTP method, dedup strategy source) where omitted.
func (c *AttackConfig) Validate() error {
	if c.Target.URL == "" {
		return fmt.Errorf("attacker: target url is required")
	}
	if c.Rate <= 0 {
		return fmt.Errorf("attacker: rate must be > 0, got %d", c.Rate)
	}
	if c.DurationS <= 0 {
		return fmt.Errorf("attacker: duration_s must be > 0, got %d", c.DurationS)
	}
	if c.Target.Method == "" {
		c.Target.Method = http.MethodGet
	}
	if c.MetaTraceID != "" {
		if err := trace.ValidateBaggageValue(c.MetaTraceID); err != nil {
			return fmt.Errorf("attacker: meta_trace_id: %w", err)
		}
	}
	if c.WorkflowLabel != "" {
		if err := trace.ValidateBaggageValue(c.WorkflowLabel); err != nil {
			return fmt.Errorf("attacker: workflow_label: %w", err)
		}
	}
	if c.DedupBypass != nil {
		switch c.DedupBypass.Strategy {
		case DedupStrategyHeader:
			if c.DedupBypass.Source == "" {
				c.DedupBypass.Source = DefaultHeaderDedupName
			}
		case DedupStrategyQuery:
			if c.DedupBypass.Source == "" {
				c.DedupBypass.Source = DefaultQueryDedupName
			}
		default:
			return fmt.Errorf("attacker: unknown dedup_bypass strategy %q (want %q or %q)",
				c.DedupBypass.Strategy, DedupStrategyHeader, DedupStrategyQuery)
		}
	}
	return nil
}

// Attack represents a running or completed vegeta attack. The atk and cancel
// fields are runtime-only and intentionally excluded from JSON output.
type Attack struct {
	Config    AttackConfig  `json:"config"`
	Status    string        `json:"status"`
	StartedAt time.Time     `json:"started_at"`
	Result    *AttackResult `json:"result,omitempty"`

	atk    *vegeta.Attacker   `json:"-"`
	cancel context.CancelFunc `json:"-"`
}

// Manager orchestrates concurrent attacks with lifecycle tracking.
type Manager struct {
	mu      sync.RWMutex
	attacks map[string]*Attack
}

// NewManager creates an attack manager.
func NewManager() *Manager {
	return &Manager{attacks: make(map[string]*Attack)}
}

// Launch starts an attack in the background and returns immediately. onComplete
// is invoked once after the goroutine finishes (natural completion, stop, or
// timeout) — callers use it to decrement gauges or hook experiment-phase logic.
// onComplete may be nil.
func (m *Manager) Launch(ctx context.Context, cfg AttackConfig, onComplete func(*Attack)) (*Attack, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	duration := cfg.Duration()
	// Slightly longer than the attack duration so the context outlives the
	// natural completion; Stop() is the real halt mechanism for early exit.
	ctx, cancel := context.WithTimeout(ctx, duration+5*time.Second)

	atk := newVegetaAttacker(cfg)
	attack := &Attack{
		Config:    cfg,
		Status:    "running",
		StartedAt: time.Now(),
		atk:       atk,
		cancel:    cancel,
	}

	m.mu.Lock()
	m.attacks[cfg.ID] = attack
	m.mu.Unlock()

	targeter := m.buildTargeter(cfg)
	pacer := vegeta.ConstantPacer{Freq: cfg.Rate, Per: time.Second}

	go func() {
		defer cancel()
		results := atk.Attack(targeter, pacer, duration, cfg.ID)

		var metrics vegeta.Metrics
		for r := range results {
			metrics.Add(r)
		}
		metrics.Close()

		m.mu.Lock()
		attack.Result = FromMetrics(&metrics)
		// If Stop() already set "stopped", leave it. Otherwise classify
		// based on whether the context was cancelled (timeout/parent cancel)
		// or completed naturally.
		if attack.Status == "running" {
			if ctx.Err() != nil {
				attack.Status = "stopped"
			} else {
				attack.Status = "completed"
			}
		}
		m.mu.Unlock()

		if onComplete != nil {
			onComplete(attack)
		}
	}()

	return attack, nil
}

// Get returns an attack by ID.
func (m *Manager) Get(id string) (*Attack, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.attacks[id]
	return a, ok
}

// Stop cancels a running attack. It halts vegeta synchronously so no further
// requests fire; the launch goroutine drains in-flight responses and finalizes
// status + result.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	a, ok := m.attacks[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("attacker: attack %q not found", id)
	}
	if a.Status != "running" {
		m.mu.Unlock()
		return fmt.Errorf("attacker: attack %q is not running (status: %s)", id, a.Status)
	}
	a.Status = "stopped"
	atk := a.atk
	cancel := a.cancel
	m.mu.Unlock()

	// Release the lock before Stop()/cancel() — vegeta.Stop drains internal
	// channels and we don't want to block other manager operations.
	if atk != nil {
		atk.Stop()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// List returns all tracked attacks.
func (m *Manager) List() []*Attack {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Attack, 0, len(m.attacks))
	for _, a := range m.attacks {
		out = append(out, a)
	}
	return out
}

// StopAll cancels all running attacks. Each one goes through Stop() so vegeta
// halts cleanly — important on SIGTERM, otherwise zeus's graceful HTTP
// shutdown leaves vegeta blasting until each attack's duration expires.
func (m *Manager) StopAll() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.attacks))
	for id, a := range m.attacks {
		if a.Status == "running" {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// newVegetaAttacker builds a vegeta.Attacker with options applied only when
// the corresponding config field is non-zero. This avoids accidentally
// overriding vegeta's defaults with sentinel zero values.
func newVegetaAttacker(cfg AttackConfig) *vegeta.Attacker {
	var opts []func(*vegeta.Attacker)
	if cfg.TimeoutS > 0 {
		opts = append(opts, vegeta.Timeout(time.Duration(cfg.TimeoutS)*time.Second))
	}
	if cfg.MaxConnections > 0 {
		opts = append(opts, vegeta.MaxConnections(cfg.MaxConnections))
	}
	if cfg.MaxBodyBytes != 0 {
		opts = append(opts, vegeta.MaxBody(cfg.MaxBodyBytes))
	}
	if cfg.Redirects != 0 {
		opts = append(opts, vegeta.Redirects(cfg.Redirects))
	}
	return vegeta.NewAttacker(opts...)
}

func (m *Manager) buildTargeter(cfg AttackConfig) vegeta.Targeter {
	base := vegeta.Target{
		Method: cfg.Target.Method,
		URL:    cfg.Target.URL,
		Body:   cfg.Target.Body,
		Header: toHTTPHeader(cfg.Target.Headers),
	}
	bypass := buildBypass(cfg.DedupBypass)

	return func(t *vegeta.Target) error {
		*t = base
		t.Header = base.Header.Clone()

		if bypass != nil {
			// Pass the original body so future body-mutator strategies can
			// rewrite it; current header/query mutators leave Body nil and
			// the branch below is a no-op for them.
			var bodyReader io.Reader
			if len(t.Body) > 0 {
				bodyReader = bytes.NewReader(t.Body)
			}
			req, err := http.NewRequest(t.Method, t.URL, bodyReader)
			if err != nil {
				return err
			}
			req.Header = t.Header
			transformed := bypass.Transform(req)
			t.Header = transformed.Header
			t.URL = transformed.URL.String()
			if transformed.Body != nil {
				body, err := io.ReadAll(transformed.Body)
				if err != nil {
					return err
				}
				t.Body = body
			}
		}

		// TODO(metrics): when per-attack dedup-effectiveness metrics land
		// (e.g. unique-key count, collision rate), emit them here — this is
		// the only place that sees both the spec and the mutated request.

		entries := make(map[string]string)
		if cfg.MetaTraceID != "" {
			entries[trace.MetaTraceKey] = cfg.MetaTraceID
		}
		if cfg.WorkflowLabel != "" {
			entries["atropos.workflow"] = cfg.WorkflowLabel
		}
		if len(entries) > 0 {
			trace.InjectLabeledBaggage(t.Header, entries)
		}
		return nil
	}
}

// buildBypass constructs the per-attack dedup mutator from spec. Validate()
// guarantees Strategy is recognized and Source has been defaulted.
func buildBypass(spec *DedupBypassSpec) dedup.DedupBypass {
	if spec == nil {
		return nil
	}
	switch spec.Strategy {
	case DedupStrategyHeader:
		return &dedup.HeaderMutator{HeaderName: spec.Source}
	case DedupStrategyQuery:
		return &dedup.QueryParamMutator{ParamName: spec.Source}
	}
	return nil
}

func toHTTPHeader(headers map[string]string) http.Header {
	h := make(http.Header, len(headers))
	for k, v := range headers {
		h.Set(k, v)
	}
	return h
}

// LogAttackResult logs a summary of attack results.
func LogAttackResult(id string, a *Attack) {
	if a.Result == nil {
		log.Printf("attack %s: no results yet (status: %s)", id, a.Status)
		return
	}
	log.Printf("attack %s: %d reqs, %.1f req/s, p50=%s p99=%s, %.0f%% success",
		id, a.Result.TotalRequests, a.Result.RateActual,
		a.Result.Latencies.P50, a.Result.Latencies.P99,
		a.Result.Success*100)
}
