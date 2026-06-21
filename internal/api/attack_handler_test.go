package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"atropos-go/loadgen/internal/attacker"
	"atropos-go/loadgen/internal/run"
	"atropos-go/loadgen/internal/stats"
)

// These structs mirror manteion-go/internal/zeus/client.go field-for-field.
// Decoding zeus's responses into them is the wire-contract assertion: if zeus
// drifts, the JSON tags here stop populating and the tests fail.
type manteionAttackInfo struct {
	ID          string     `json:"id"`
	WorkloadID  string     `json:"workload_id"`
	Service     string     `json:"service"`
	Status      string     `json:"status"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type manteionAttackResultInfo struct {
	AttackID      string  `json:"attack_id"`
	Service       string  `json:"service"`
	TotalRequests int64   `json:"total_requests"`
	DurationMs    int64   `json:"duration_ms"`
	RateActual    float64 `json:"rate_actual"`
	SuccessRate   float64 `json:"success_rate"`
	LatencyP50Us  int64   `json:"latency_p50_us"`
	LatencyP90Us  int64   `json:"latency_p90_us"`
	LatencyP95Us  int64   `json:"latency_p95_us"`
	LatencyP99Us  int64   `json:"latency_p99_us"`
	ThroughputRPS float64 `json:"throughput_rps"`
}

func newAttackTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(Deps{
		Attacks: attacker.NewManager(),
		Runs:    run.NewStore(),
		Metrics: stats.NewMetrics(),
	})
}

func doJSON(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// TestCreateAttackReturns201 locks the StartAttack status code: manteion's
// client accepts only 200/201, so a 202 (the old behavior) breaks it.
func TestCreateAttackReturns201(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	s := newAttackTestServer(t)

	body := `{"id":"atk-201","target":{"url":"` + target.URL + `","method":"GET"},"rate":10,"duration_s":1}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/attacks", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.ID != "atk-201" || env.Status != "running" {
		t.Errorf("envelope = %+v, want id=atk-201 status=running", env)
	}
}

// TestCreateAttackIgnoresRunRef proves the run_ref blocker is gone: a body that
// still carries run_ref (a stale client) is accepted, not 400'd.
func TestCreateAttackIgnoresRunRef(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	s := newAttackTestServer(t)

	// run_ref references a run that does not exist — old zeus returned 400 here.
	body := `{"id":"atk-rr","target":{"url":"` + target.URL + `","method":"GET"},"rate":5,"duration_s":1,"run_ref":"phase-does-not-exist"}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/attacks", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (run_ref must be ignored); body = %s", rec.Code, rec.Body.String())
	}
}

// TestAttackInfoAndResultShapes drives an attack to completion and decodes the
// GET /attacks/{id} and /result responses into the manteion mirror structs.
func TestAttackInfoAndResultShapes(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	s := newAttackTestServer(t)

	body := `{"id":"atk-shape","target":{"url":"` + target.URL + `","method":"GET"},"rate":20,"duration_s":1}`
	if rec := doJSON(t, s, http.MethodPost, "/api/v1/attacks", body); rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d; body = %s", rec.Code, rec.Body.String())
	}

	// While running, result polls 404 (manteion reads this as "not ready").
	if rec := doJSON(t, s, http.MethodGet, "/api/v1/attacks/atk-shape/result", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("result while running: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	// GET /attacks/{id} must decode as AttackInfo with a TOP-LEVEL id (not nested
	// under "config") — the old shape left manteion's info.ID empty.
	rec := doJSON(t, s, http.MethodGet, "/api/v1/attacks/atk-shape", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get attack: status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"config"`) {
		t.Errorf("GetAttack still returns the raw Attack (has \"config\"): %s", rec.Body.String())
	}
	var info manteionAttackInfo
	if err := json.NewDecoder(rec.Body).Decode(&info); err != nil {
		t.Fatalf("decode AttackInfo: %v", err)
	}
	if info.ID != "atk-shape" || info.Status == "" || info.StartedAt == nil {
		t.Errorf("AttackInfo = %+v, want id=atk-shape, non-empty status, started_at set", info)
	}

	// Poll to completion, then assert AttackResultInfo populates.
	deadline := time.Now().Add(6 * time.Second)
	var resultRec *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		resultRec = doJSON(t, s, http.MethodGet, "/api/v1/attacks/atk-shape/result", "")
		if resultRec.Code == http.StatusOK {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if resultRec.Code != http.StatusOK {
		t.Fatalf("result never ready; last status = %d, body = %s", resultRec.Code, resultRec.Body.String())
	}
	var res manteionAttackResultInfo
	if err := json.NewDecoder(resultRec.Body).Decode(&res); err != nil {
		t.Fatalf("decode AttackResultInfo: %v", err)
	}
	if res.AttackID != "atk-shape" || res.TotalRequests == 0 || res.DurationMs == 0 {
		t.Errorf("AttackResultInfo = %+v, want attack_id set, requests>0, duration_ms>0", res)
	}
	if res.SuccessRate == 0 || res.LatencyP50Us == 0 {
		t.Errorf("AttackResultInfo latency/success unpopulated: %+v", res)
	}

	// And the completed AttackInfo carries completed_at + status "completed".
	rec = doJSON(t, s, http.MethodGet, "/api/v1/attacks/atk-shape", "")
	_ = json.NewDecoder(rec.Body).Decode(&info)
	if info.Status != "completed" || info.CompletedAt == nil {
		t.Errorf("after completion AttackInfo = %+v, want status=completed + completed_at", info)
	}
}
