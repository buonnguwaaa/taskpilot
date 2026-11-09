package store

import "testing"

func TestStaleTakeoverIsIgnored(t *testing.T) {
	s, _ := New(t.TempDir())
	tk := &Task{ID: "mock-x"}
	tk.Transition(StateNeedsAttention, "")
	s.SaveTask(tk)
	if err := s.SetTakeover("mock-x", true); err != nil || !s.TakenOver("mock-x") {
		t.Fatalf("live takeover not seen: %v", err)
	}
	// A row from a process that no longer exists (e.g. the herdr pane was closed).
	if _, err := s.DB.Exec(`UPDATE takeovers SET pid = 999999999 WHERE task_id = 'mock-x'`); err != nil {
		t.Fatal(err)
	}
	if s.TakenOver("mock-x") {
		t.Fatal("stale takeover must not block the daemon")
	}
	s.SetTakeover("mock-x", false)
	if s.TakenOver("mock-x") {
		t.Fatal("cleared")
	}
}
