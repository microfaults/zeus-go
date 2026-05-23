package attacker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		cfg       AttackConfig
		wantErr   string
		wantAfter func(*AttackConfig) bool
	}{
		{
			name:    "missing url",
			cfg:     AttackConfig{Rate: 10, DurationS: 5},
			wantErr: "target url is required",
		},
		{
			name:    "rate zero",
			cfg:     AttackConfig{Target: TargetSpec{URL: "http://x"}, DurationS: 5},
			wantErr: "rate must be > 0",
		},
		{
			name:    "duration zero",
			cfg:     AttackConfig{Target: TargetSpec{URL: "http://x"}, Rate: 10},
			wantErr: "duration_s must be > 0",
		},
		{
			name: "default method becomes GET",
			cfg:  AttackConfig{Target: TargetSpec{URL: "http://x"}, Rate: 10, DurationS: 5},
			wantAfter: func(c *AttackConfig) bool {
				return c.Target.Method == http.MethodGet
			},
		},
		{
			name: "meta_trace_id with comma rejected",
			cfg: AttackConfig{
				Target:      TargetSpec{URL: "http://x"},
				Rate:        10,
				DurationS:   5,
				MetaTraceID: "abc,def",
			},
			wantErr: "meta_trace_id",
		},
		{
			name: "workflow_label with whitespace rejected",
			cfg: AttackConfig{
				Target:        TargetSpec{URL: "http://x"},
				Rate:          10,
				DurationS:     5,
				WorkflowLabel: "browse run",
			},
			wantErr: "workflow_label",
		},
		{
			name: "dedup header strategy defaults Source",
			cfg: AttackConfig{
				Target:      TargetSpec{URL: "http://x"},
				Rate:        10,
				DurationS:   5,
				DedupBypass: &DedupBypassSpec{Strategy: DedupStrategyHeader},
			},
			wantAfter: func(c *AttackConfig) bool {
				return c.DedupBypass.Source == DefaultHeaderDedupName
			},
		},
		{
			name: "dedup query strategy defaults Source",
			cfg: AttackConfig{
				Target:      TargetSpec{URL: "http://x"},
				Rate:        10,
				DurationS:   5,
				DedupBypass: &DedupBypassSpec{Strategy: DedupStrategyQuery},
			},
			wantAfter: func(c *AttackConfig) bool {
				return c.DedupBypass.Source == DefaultQueryDedupName
			},
		},
		{
			name: "dedup explicit Source preserved",
			cfg: AttackConfig{
				Target:      TargetSpec{URL: "http://x"},
				Rate:        10,
				DurationS:   5,
				DedupBypass: &DedupBypassSpec{Strategy: DedupStrategyHeader, Source: "X-Request-ID"},
			},
			wantAfter: func(c *AttackConfig) bool {
				return c.DedupBypass.Source == "X-Request-ID"
			},
		},
		{
			name: "dedup unknown strategy rejected",
			cfg: AttackConfig{
				Target:      TargetSpec{URL: "http://x"},
				Rate:        10,
				DurationS:   5,
				DedupBypass: &DedupBypassSpec{Strategy: "body"},
			},
			wantErr: `unknown dedup_bypass strategy "body"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantAfter != nil && !tc.wantAfter(&tc.cfg) {
				t.Fatalf("post-condition failed; cfg=%+v", tc.cfg)
			}
		})
	}
}

func TestLaunchOnCompleteFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewManager()
	done := make(chan *Attack, 1)
	cfg := AttackConfig{
		ID:        "t1",
		Target:    TargetSpec{URL: srv.URL, Method: http.MethodGet},
		Rate:      20,
		DurationS: 1,
	}
	if _, err := m.Launch(context.Background(), cfg, func(a *Attack) { done <- a }); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case a := <-done:
		if a.Status != "completed" {
			t.Fatalf("expected status completed, got %s", a.Status)
		}
		if a.Result == nil || a.Result.TotalRequests == 0 {
			t.Fatalf("expected non-empty result, got %+v", a.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onComplete never fired")
	}
}

// TestStopActuallyHaltsVegeta launches a high-RPS attack against an httptest
// server with a request counter, calls Stop mid-flight, and asserts vegeta
// halts well before the configured duration. Pre-fix, Stop only flipped status
// while vegeta kept firing — the counter would have approached rate * duration.
func TestStopActuallyHaltsVegeta(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewManager()
	cfg := AttackConfig{
		ID:        "stop-test",
		Target:    TargetSpec{URL: srv.URL, Method: http.MethodGet},
		Rate:      100,
		DurationS: 10, // would emit ~1000 hits if Stop is broken
	}
	if _, err := m.Launch(context.Background(), cfg, nil); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Let it run briefly, then stop.
	time.Sleep(500 * time.Millisecond)
	if err := m.Stop("stop-test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Drain in-flight + small slack window.
	time.Sleep(1 * time.Second)

	total := hits.Load()
	// At 100 RPS for the configured 10 s we'd expect ~1000 hits. With Stop
	// working correctly we should see roughly 0.5 s of traffic plus a drain
	// tail — well under 200.
	if total >= 200 {
		t.Fatalf("Stop did not halt vegeta: got %d hits (want < 200, configured for %d s)",
			total, cfg.DurationS)
	}

	a, ok := m.Get("stop-test")
	if !ok {
		t.Fatal("attack missing after Stop")
	}
	if a.Status != "stopped" {
		t.Fatalf("expected status stopped, got %s", a.Status)
	}
}

func TestDedupBypassHeaderUniquifies(t *testing.T) {
	var seen sync.Map
	var unique atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-Idempotency-Key")
		if k == "" {
			return
		}
		if _, loaded := seen.LoadOrStore(k, struct{}{}); !loaded {
			unique.Add(1)
		}
	}))
	defer srv.Close()

	m := NewManager()
	done := make(chan *Attack, 1)
	cfg := AttackConfig{
		ID:          "dedup-test",
		Target:      TargetSpec{URL: srv.URL, Method: http.MethodGet},
		Rate:        20,
		DurationS:   1,
		DedupBypass: &DedupBypassSpec{Strategy: DedupStrategyHeader},
	}
	if _, err := m.Launch(context.Background(), cfg, func(a *Attack) { done <- a }); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attack did not complete in time")
	}

	// Header bypass should produce a unique value per request.
	if u := unique.Load(); u < 10 {
		t.Fatalf("expected >= 10 unique idempotency keys, got %d", u)
	}
}
