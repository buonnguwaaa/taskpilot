package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestAttachHasakiTaskToRunningTask(t *testing.T) {
	m, st, _ := newTest(t)
	host := &store.Task{ID: "hw-HSK-HOST", Title: "Replace SKU theo màu", Project: "bom", Branch: "feat/replace-sku"}
	host.Transition(store.StateNeedsAttention, "")
	st.SaveTask(host)
	m.reloadTasks()

	tm, cmd := m.switchTab(tabHasaki)
	mm := drain(t, tm.(Model), cmd)
	mm.hw.cursor = 0
	mm, _ = press(t, mm, "a")
	if mm.modal == nil || mm.modal.value("host") != host.ID {
		t.Fatalf("attach form should open on the current task: modal=%v msg=%q", mm.modal != nil, mm.msg)
	}
	mm, _ = press(t, mm, "tab")
	mm = typeText(mm, "bug này của màn style")
	mm, cmd2 := press(t, mm, "ctrl+s")
	if mm.modal != nil {
		t.Fatalf("form error: %s", mm.modal.err)
	}
	mm = drain(t, mm, cmd2)
	if !strings.Contains(mm.msg, "đã gộp HSK-AAA vào task hw-HSK-HOST") {
		t.Fatalf("msg = %q", mm.msg)
	}
	msgs, _ := st.OperatorMessages(host.ID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Mô tả của task HSK-AAA") ||
		!strings.Contains(msgs[0].Text, "feat/replace-sku") || !strings.Contains(msgs[0].Text, "bug này của màn style") {
		t.Fatalf("message to the agent: %+v", msgs)
	}
	if id, _ := st.TicketHost("hw", "HSK-AAA"); id != host.ID {
		t.Fatalf("ticket host = %q", id)
	}
	// The ticket now shows as worked in the host; Enter jumps there.
	if !strings.Contains(ansi.Strip(mm.View()), "Đã giao cho agent: task hw-HSK-HOST") {
		t.Fatalf("detail:\n%s", ansi.Strip(mm.View()))
	}
	mm, _ = press(t, mm, "enter")
	if mm.tab != tabTasks || mm.tasks.current().ID != host.ID {
		t.Fatalf("enter should open the host task: tab=%d", mm.tab)
	}
	if !strings.Contains(ansi.Strip(mm.View()), "Gộp thêm ticket HSK-AAA") {
		t.Fatalf("host detail:\n%s", ansi.Strip(mm.View()))
	}
}
