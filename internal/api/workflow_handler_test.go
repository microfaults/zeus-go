package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"atropos-go/loadgen/internal/stats"
	"atropos-go/loadgen/internal/workflow"
)

func newValidateTestServer(t *testing.T) (*Server, *workflow.Store) {
	t.Helper()
	wfs := workflow.NewStore()
	// Metrics is required by routes() (the /metrics mount reads its registry).
	return NewServer(Deps{Workflows: wfs, Metrics: stats.NewMetrics()}), wfs
}

func postValidate(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/validate", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

const validDoc = `{"workflow":{"name":"browse","version":"2","targets":["frontend"],
	"estimated_rps_per_vu":2,"root":{"type":"request","id":"home","path":"/","method":"GET"}}}`

func TestValidateWorkflowDoc_Valid(t *testing.T) {
	s, wfs := newValidateTestServer(t)

	rec := postValidate(t, s, validDoc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.Name != "browse" {
		t.Errorf("resp = %+v, want ok=true name=browse", resp)
	}

	// Statelessness: validation must not register anything.
	if got := wfs.List(); len(got) != 0 {
		t.Errorf("validate registered a workflow: %+v", got)
	}
}

func TestValidateWorkflowDoc_BadVersion(t *testing.T) {
	s, _ := newValidateTestServer(t)
	body := `{"workflow":{"name":"w","version":"1","targets":["s"],
		"estimated_rps_per_vu":1,"root":{"type":"request"}}}`
	rec := postValidate(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "version") {
		t.Errorf("error should mention version, got %s", rec.Body.String())
	}
}

func TestValidateWorkflowDoc_NodeCapExceeded(t *testing.T) {
	s, _ := newValidateTestServer(t)

	// Build a sequence with >100 request nodes to trip the maxNodes cap.
	kids := make([]string, 0, 101)
	for i := 0; i < 101; i++ {
		kids = append(kids, fmt.Sprintf(`{"type":"request","id":"r%d","path":"/","method":"GET"}`, i))
	}
	body := fmt.Sprintf(`{"workflow":{"name":"big","version":"2","targets":["s"],
		"estimated_rps_per_vu":1,"root":{"type":"sequence","children":[%s]}}}`,
		strings.Join(kids, ","))

	rec := postValidate(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestValidateWorkflowDoc_MissingEnvelope(t *testing.T) {
	s, _ := newValidateTestServer(t)
	rec := postValidate(t, s, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workflow field is required") {
		t.Errorf("unexpected error body: %s", rec.Body.String())
	}
}
