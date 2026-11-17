package daemon

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestPermissionAnswerGrantsAndResumes(t *testing.T) {
	st, _ := store.New(t.TempDir())
	d := &Daemon{Config: &config.Config{}, Store: st, Log: log.New(io.Discard, "", 0)}
	ask := func(id, rule, answer string) *store.Task {
		tk := &store.Task{ID: id, Project: "bom"}
		tk.Transition(store.StateAwaitApproval, "")
		st.SaveTask(tk)
		qs := []store.Question{{Question: "Cho agent chạy?", Options: []store.Option{{Label: store.PermAllowTask}, {Label: store.PermAllowProject}, {Label: store.PermDeny}}}}
		r, err := st.AddRequestWithQuestions(id, store.QuestionGate, "xin quyền", "", qs)
		if err != nil {
			t.Fatal(err)
		}
		st.SetPermissionRequest(r.ID, id, rule)
		st.Decide(id, r, true, "Q1: Cho agent chạy?\n→ "+answer+"\n")
		tk, _ = st.LoadTask(id)
		if err := d.advance(tk); err != nil {
			t.Fatal(err)
		}
		tk, _ = st.LoadTask(id)
		return tk
	}
	tk := ask("mock-p1", "Bash(curl *)", store.PermAllowTask)
	if tk.State != store.StateQueued || !strings.Contains(tk.NextPrompt, "allowed Bash(curl *) for this task") {
		t.Fatalf("task grant: %s %q", tk.State, tk.NextPrompt)
	}
	gs, _ := st.Grants("mock-p1", "bom")
	if len(gs) != 1 || gs[0].Scope != store.GrantTask {
		t.Fatalf("grants = %+v", gs)
	}
	tk = ask("mock-p2", "Bash(jq *)", store.PermAllowProject)
	if gs, _ := st.Grants("other-task", "bom"); len(gs) != 1 || gs[0].Rule != "Bash(jq *)" || !strings.Contains(tk.NextPrompt, "every task of project bom") {
		t.Fatalf("project grant: %+v %q", gs, tk.NextPrompt)
	}
	tk = ask("mock-p3", "Bash(docker *)", store.PermDeny)
	if !strings.Contains(tk.NextPrompt, "did NOT allow Bash(docker *)") {
		t.Fatalf("denied: %q", tk.NextPrompt)
	}
	if gs, _ := st.Grants("mock-p3", "bom"); len(gs) != 1 { // only bom's jq
		t.Fatalf("a refusal grants nothing: %+v", gs)
	}
}
