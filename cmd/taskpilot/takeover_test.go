package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestTakeoverMarksWhileClaudeRuns(t *testing.T) {
	st, _ := store.New(t.TempDir())
	ws := t.TempDir()
	tk := &store.Task{ID: "mock-t", Title: "x", Workspace: ws, Runs: 1, SessionID: "sid-1"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	// Fake claude: records its args, cwd and whether the marker exists while it runs.
	out := filepath.Join(t.TempDir(), "seen")
	claude := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n{ echo \"$*\"; pwd; python3 -I -c 'import sqlite3,sys; print(\"marked\" if sqlite3.connect(sys.argv[1]).execute(\"select count(*) from takeovers where task_id=?\",(\"mock-t\",)).fetchone()[0] else \"unmarked\")' " +
		filepath.Join(st.Home, "taskpilot.db") + "; } > " + out + "\n"
	os.WriteFile(claude, []byte(script), 0o755)

	cfg := &config.Config{Agent: config.AgentClaude, Claude: config.Claude{Bin: claude}}
	if err := takeover(st, cfg, "mock-t"); err != nil {
		t.Fatal(err)
	}
	seen, _ := os.ReadFile(out)
	if s := string(seen); !strings.Contains(s, "--resume sid-1") || !strings.Contains(s, ws) || !strings.Contains(s, "marked") {
		t.Fatalf("claude saw: %q", s)
	}
	if st.TakenOver("mock-t") {
		t.Fatal("marker must be cleared after claude exits")
	}

	// Refused while the task runs, or while someone else drives it.
	tk.Transition(store.StateRunning, "")
	st.SaveTask(tk)
	if err := takeover(st, cfg, "mock-t"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running task: %v", err)
	}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	st.SetTakeover("mock-t", true) // this (live) process holds it
	if err := takeover(st, cfg, "mock-t"); err == nil || !strings.Contains(err.Error(), "another pane") {
		t.Fatalf("double takeover: %v", err)
	}
}
