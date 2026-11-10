package tui

import (
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestMarkDoneByHand(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-A", Title: "Sửa export", Project: "bom", LastError: "boom"}
	tk.Transition(store.StateAwaitApproval, "")
	st.SaveTask(tk)
	req, err := st.AddRequest("mock-A", "pr", "tạo PR?", "")
	if err != nil {
		t.Fatal(err)
	}
	m.reloadTasks()

	m, _ = press(t, m, "c")
	if m.modal == nil || !strings.Contains(m.modal.hint, req.ID) {
		t.Fatal("c opens the form and says the pending request will be closed")
	}
	m = typeText(m, "Đã sửa tay và merge")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form error: %s", m.modal.err)
	}
	got, _ := st.LoadTask("mock-A")
	if got.State != store.StateDone || got.LastError != "" {
		t.Fatalf("state=%s err=%q", got.State, got.LastError)
	}
	if p, _ := st.PendingRequest("mock-A"); p != nil {
		t.Fatal("the pending request must be closed")
	}
	if c, _ := st.Completion("mock-A"); c == nil || c.Summary != "Đã sửa tay và merge" {
		t.Fatalf("completion = %+v", c)
	}
	if !strings.Contains(m.View(), "Đã xong (1)") {
		t.Fatal("the task moves to the done group")
	}
	m, _ = press(t, m, "c")
	if m.modal != nil || !strings.Contains(m.msg, "đã xong") {
		t.Fatal("already done: nothing to do")
	}
}

func TestMarkDoneRefusedWhileRunningAndDefaultNote(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-B", Title: "x", Project: "bom"}
	tk.Transition(store.StateQueued, "")
	st.SaveTask(tk)
	m.reloadTasks()
	if m, _ = press(t, m, "c"); m.modal != nil {
		t.Fatal("a queued task is about to run: refuse")
	}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "c")
	m, _ = press(t, m, "ctrl+s")
	if c, _ := st.Completion("mock-B"); c == nil || c.Summary != manualDoneNote {
		t.Fatalf("empty note → default summary, got %+v", c)
	}
}
