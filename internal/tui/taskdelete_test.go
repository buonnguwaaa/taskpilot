package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestDeleteTask(t *testing.T) {
	m, st, inbox := newTest(t)
	if err := inbox.Submit(source.Ticket{ID: "T1", Title: "x", Project: "bom"}); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(ws, 0o755)
	tk := &store.Task{ID: "mock-T1", TicketID: "T1", Title: "Sửa export", Project: "bom", Workspace: ws, Branch: "tp/mock-T1"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	other := &store.Task{ID: "mock-T2", Title: "giữ lại", Project: "bom"}
	other.Transition(store.StateDone, "")
	st.SaveTask(other)
	m.reloadTasks()
	for m.tasks.current().ID != "mock-T1" {
		m, _ = press(t, m, "down")
	}

	m, _ = press(t, m, "d")
	if m.conf == nil || !strings.Contains(m.conf.text, "mock-T1") || !strings.Contains(m.conf.text, "CHƯA commit") || !strings.Contains(m.conf.text, "tp/mock-T1") {
		t.Fatalf("d must ask first, saying what goes and what stays: %+v", m.conf)
	}
	if !st.Exists("mock-T1") {
		t.Fatal("nothing is deleted before y")
	}
	m, cmd := press(t, m, "y")
	m = run(t, m, cmd)

	if st.Exists("mock-T1") || !st.Exists("mock-T2") {
		t.Fatal("only the selected task must be deleted")
	}
	if _, err := os.Stat(filepath.Join(inbox.Dir, "T1.json")); !os.IsNotExist(err) {
		t.Fatal("the inbox ticket must go, or the daemon picks the task up again")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("the workspace must be removed")
	}
	if !strings.Contains(m.msg, "đã xoá mock-T1") || ids(m) != "mock-T2" {
		t.Fatalf("msg=%q list=%s", m.msg, ids(m))
	}
}

func TestDeleteTaskCancelledOrRunning(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-R", Title: "đang chạy", Project: "bom"}
	tk.Transition(store.StateRunning, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "d")
	if m.conf != nil || !strings.Contains(m.msg, "đang chạy") {
		t.Fatalf("a running task cannot be deleted: conf=%v msg=%q", m.conf != nil, m.msg)
	}

	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "d")
	m, _ = press(t, m, "n")
	if m.conf != nil || !st.Exists("mock-R") {
		t.Fatal("n cancels")
	}
}
