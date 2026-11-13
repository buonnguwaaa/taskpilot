package store

import (
	"testing"
	"time"
)

func TestTaskWorkerAndQuotaLatch(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tk := &Task{ID: "mock-1", Title: "x"}
	tk.Transition(StateQueued, "")
	if err := s.SaveTask(tk); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.TaskWorker("mock-1"); w != nil {
		t.Fatal("no worker yet")
	}
	if err := s.SetTaskWorker("mock-1", "cursor", "weekly limit"); err != nil {
		t.Fatal(err)
	}
	w, err := s.TaskWorker("mock-1")
	if err != nil || w.Agent != "cursor" || w.Reason != "weekly limit" {
		t.Fatalf("%+v %v", w, err)
	}

	if latch, _ := s.ClaudeQuotaLatch(); latch != nil {
		t.Fatal("no latch yet")
	}
	until := time.Now().UTC().Add(2 * time.Hour)
	if err := s.SetClaudeQuotaLatch(until, "weekly limit"); err != nil {
		t.Fatal(err)
	}
	latch, err := s.ClaudeQuotaLatch()
	if err != nil || latch == nil || latch.Reason != "weekly limit" || latch.Resets.Before(time.Now()) {
		t.Fatalf("%+v %v", latch, err)
	}
	if err := s.ClearClaudeQuotaLatch(); err != nil {
		t.Fatal(err)
	}
	if latch, _ := s.ClaudeQuotaLatch(); latch != nil {
		t.Fatal("cleared")
	}
}
