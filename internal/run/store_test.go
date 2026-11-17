package run

import "testing"

// TestStore_DeleteRunsEvictHooks: evicting a run record runs the OnEvict
// hooks (zeus wires Prometheus series and snapshot cleanup to them).
func TestStore_DeleteRunsEvictHooks(t *testing.T) {
	s := NewStore()
	if err := s.Create(&Run{ID: "r1", WorkflowID: "wf"}); err != nil {
		t.Fatal(err)
	}
	var evicted []string
	s.OnEvict(func(id string) {
		// Hooks run outside the store lock: re-entering the store is safe.
		if _, ok := s.Get(id); ok {
			t.Error("record still present when hook runs")
		}
		evicted = append(evicted, id)
	})
	if err := s.Delete("r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("r1"); err == nil {
		t.Fatal("second delete must fail")
	}
	if len(evicted) != 1 || evicted[0] != "r1" {
		t.Fatalf("evicted = %v, want [r1]", evicted)
	}
}
