package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestOpenWorkspaceInIDE(t *testing.T) {
	m, st, _ := newTest(t)
	var opened []string
	m.d.OpenEditor = func(dir string) error { opened = append(opened, dir); return nil }
	ws := t.TempDir()
	for _, r := range []string{"bom-api", "bom-worker"} {
		os.MkdirAll(filepath.Join(ws, r), 0o755)
		os.WriteFile(filepath.Join(ws, r, ".git"), []byte("gitdir: x"), 0o644) // a worktree
	}
	tk := &store.Task{ID: "mock-w", Title: "x", Project: "bom", Workspace: ws, Branch: "tp/mock-w"}
	tk.Transition(store.StateRunning, "")
	st.SaveTask(tk)
	m.reloadTasks()

	if !strings.Contains(m.tasks.lastBody, "w mở trong IDE · W xem trong repo của bạn") {
		t.Fatal("the detail pane hints at w")
	}
	m, _ = press(t, m, "w")
	if len(opened) != 1 || opened[0] != ws || !strings.Contains(m.msg, "bom-api, bom-worker") || !strings.Contains(m.msg, "đừng sửa") {
		t.Fatalf("opened=%v msg=%q", opened, m.msg)
	}
	// One repo: open it directly.
	os.RemoveAll(filepath.Join(ws, "bom-worker"))
	m, _ = press(t, m, "w")
	if opened[1] != filepath.Join(ws, "bom-api") {
		t.Fatalf("single repo: %v", opened)
	}
	// Released (task done): explain where the branch is.
	os.RemoveAll(filepath.Join(ws, "bom-api"))
	m, _ = press(t, m, "w")
	if len(opened) != 2 || !strings.Contains(m.msg, "tp/mock-w") {
		t.Fatalf("released workspace: opened=%v msg=%q", opened, m.msg)
	}
}

func TestOpenEditorNeedsACommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := openEditor("", t.TempDir()); err == nil || !strings.Contains(err.Error(), "editor:") {
		t.Fatalf("no IDE in PATH: %v", err)
	}
}

func TestReviewTargetsUseTheWorktreeBranch(t *testing.T) {
	m, st, _ := newTest(t)
	dir := t.TempDir()
	clone := filepath.Join(dir, "bom-api")
	for _, args := range [][]string{{"init", "-q", "-b", "main", clone}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s", out)
		}
	}
	git := func(d string, a ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@t"}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", a, out)
		}
	}
	os.WriteFile(filepath.Join(clone, "a"), []byte("1"), 0o644)
	git(clone, "add", ".")
	git(clone, "commit", "-q", "-m", "one")
	ws := filepath.Join(dir, "ws")
	git(clone, "worktree", "add", "-q", "-b", "feat/renamed-by-agent", filepath.Join(ws, "bom-api"))
	p := m.d.Config.Projects["bom"]
	p.Repos = map[string]config.Repo{"bom-api": {Path: clone}}
	m.d.Config.Projects["bom"] = p
	tk := &store.Task{ID: "mock-v", Title: "x", Project: "bom", Workspace: ws, Branch: "tp/mock-v"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)

	got := m.reviewTargets(tk)
	if len(got) != 1 || got[0].branch != "feat/renamed-by-agent" || got[0].clone != clone {
		t.Fatalf("targets = %+v", got)
	}
	m.reloadTasks()
	m, cmd := press(t, m, "W")
	if cmd == nil {
		t.Fatal("W runs the checkout")
	}
	tm, _ := m.Update(cmd())
	if msg := tm.(Model).msg; !strings.Contains(msg, "bom-api: đang xem feat/renamed-by-agent (detached)") {
		t.Fatalf("msg = %q", msg)
	}
}
