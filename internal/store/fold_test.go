package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFoldUsage(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task := func(id, level string, cost float64, calls ...ToolCall) {
		tk := &Task{ID: id, Project: "bom"}
		tk.Transition(StateRunning, "")
		st.SaveTask(tk)
		if level != "" {
			st.SetAssessment(id, &Assessment{Level: level})
		}
		rid, _ := st.StartRun(id, RunStart{N: 1, SessionID: "s"})
		for i := range calls {
			calls[i].Seq = i + 1
		}
		if err := st.FinishRun(rid, RunEnd{CostUSD: cost}, calls); err != nil {
			t.Fatal(err)
		}
	}
	// Light, folded: as the skill wants.
	task("mock-l1", "light", 1)
	// Light that spawned an explorer: against the rule.
	task("mock-l2", "light", 2, ToolCall{Name: "Agent", ToolUseID: "a1", InputSummary: "taskpilot:explorer sweep"})
	// Standard with a reviewer before the pr gate (type read from the log for an old call).
	task("mock-s1", "standard", 6, ToolCall{Name: "Agent", ToolUseID: "a2", InputSummary: ""})
	os.MkdirAll(st.TaskDir("mock-s1"), 0o755)
	os.WriteFile(filepath.Join(st.TaskDir("mock-s1"), "events.jsonl"), []byte(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a2","name":"Agent","input":{"subagent_type":"taskpilot:reviewer","prompt":"x"}}]}}`+"\n"), 0o644)
	time.Sleep(5 * time.Millisecond)
	req, _ := st.AddRequest("mock-s1", "pr", "PR", "")
	st.Decide("mock-s1", req, true, "")
	// Hard, folded, reached pr without a reviewer, one rejection.
	task("mock-h1", "hard", 4)
	r1, _ := st.AddRequest("mock-h1", "plan", "plan", "")
	st.Decide("mock-h1", r1, false, "sửa")
	r2, _ := st.AddRequest("mock-h1", "pr", "PR", "")
	st.Decide("mock-h1", r2, true, "")

	fs, err := st.FoldUsage(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fs.Tasks != 4 || fs.Folded != 2 || fs.ByType["explorer"] != 1 || fs.ByType["reviewer"] != 1 {
		t.Fatalf("fold = %+v", fs)
	}
	if len(fs.LightSpawned) != 1 || fs.LightSpawned[0] != "mock-l2" {
		t.Fatalf("light spawned = %v", fs.LightSpawned)
	}
	if fs.NeedReview != 2 || fs.Reviewed != 1 || len(fs.Unreviewed) != 1 || fs.Unreviewed[0] != "mock-h1" {
		t.Fatalf("review: need=%d reviewed=%d missing=%v", fs.NeedReview, fs.Reviewed, fs.Unreviewed)
	}
	if fs.With.Tasks != 1 || fs.With.Cost != 6 || fs.Without.Tasks != 1 || fs.Without.Rejected != 1 || fs.Without.Decided != 2 {
		t.Fatalf("with=%+v without=%+v", fs.With, fs.Without)
	}
	if len(fs.Levels) != 3 || fs.Levels[0].Level != "light" || fs.Levels[0].Tasks != 2 || fs.Levels[0].WithSub != 1 {
		t.Fatalf("levels = %+v", fs.Levels)
	}
}
