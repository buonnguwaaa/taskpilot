package store

import (
	"testing"
	"time"
)

func TestGraphUsage(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(task string, n int, cost float64, calls ...ToolCall) {
		tk := &Task{ID: task, Project: "bom"}
		tk.Transition(StateRunning, "")
		st.SaveTask(tk)
		id, err := st.StartRun(task, RunStart{N: n, SessionID: "s"})
		if err != nil {
			t.Fatal(err)
		}
		for i := range calls {
			calls[i].Seq = i + 1
		}
		if err := st.FinishRun(id, RunEnd{CostUSD: cost}, calls); err != nil {
			t.Fatal(err)
		}
	}
	// With the graph: one lookup in planning.
	run("mock-a", 1, 1.0,
		ToolCall{Name: "mcp__taskpilot__graphify", InputSummary: "links GenerateWorkOrderV2"},
		ToolCall{Name: "mcp__taskpilot__graphify", InputSummary: "affected bom-report::x"},
		ToolCall{Name: "Read", InputSummary: "a.go"})
	run("mock-a", 2, 3.0, ToolCall{Name: "Grep", InputSummary: "x"}) // after the plan: not counted
	// Without: Grep, Glob, a search command, a subagent; a build command is not a lookup.
	run("mock-b", 1, 2.0,
		ToolCall{Name: "Grep", InputSummary: "x"}, ToolCall{Name: "Glob", InputSummary: "**/*.go"},
		ToolCall{Name: "Bash", InputSummary: "cd bom-api && rg -n Foo"}, ToolCall{Name: "Bash", InputSummary: "go test ./..."},
		ToolCall{Name: "Agent", InputSummary: ""}, ToolCall{Name: "Read", InputSummary: "b.go"})
	run("mock-c", 1, 4.0, ToolCall{Name: "Shell", InputSummary: "git -C bom-api log -5"})

	u, err := st.GraphUsage(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if u.Calls != 2 || u.ByAction["links"] != 1 || u.ByAction["affected"] != 1 || u.Tasks != 3 || u.GraphTasks != 1 {
		t.Fatalf("usage = %+v", u)
	}
	if u.With.Tasks != 1 || u.With.Lookups != 1 || u.With.PlanCost != 1.0 {
		t.Fatalf("with = %+v", u.With)
	}
	if u.Without.Tasks != 2 || u.Without.Lookups != 2.5 || u.Without.Explorers != 0.5 || u.Without.PlanCost != 3.0 {
		t.Fatalf("without = %+v", u.Without)
	}
	// Runs that searched: mock-a 2 (Grep), mock-b 1 (Grep, rg) — mock-a 1 read only, mock-c a git log.
	if u.SearchRuns != 2 || u.GraphRuns != 0 {
		t.Fatalf("search runs = %d, graph runs = %d", u.SearchRuns, u.GraphRuns)
	}
	if len(u.Recent) != 2 || u.Recent[1] != "mock-a: affected bom-report::x" {
		t.Fatalf("recent = %v", u.Recent)
	}
	if u, _ := st.GraphUsage(time.Now().Add(time.Hour)); u.Tasks != 0 || u.Calls != 0 {
		t.Fatalf("window: %+v", u)
	}
}
