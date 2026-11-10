package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// A message with a referenced file reaches the agent with a readable copy.
func TestNudgeWithAttachedFilePickedWithCtrlO(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-r", Title: "x", Project: "bom"}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()

	docs := t.TempDir()
	spec := filepath.Join(docs, "FPTPAY spec.pdf")
	os.WriteFile(spec, []byte("%PDF spec"), 0o644)
	lastPickDir = docs
	t.Cleanup(func() { lastPickDir = "" })

	m, _ = press(t, m, "m")
	m = typeText(m, "đọc spec này")
	m, _ = press(t, m, "ctrl+o")
	if m.modal.picker == nil || !strings.Contains(m.View(), "FPTPAY spec.pdf") {
		t.Fatalf("Ctrl+O opens the picker in %s:\n%s", docs, m.View())
	}
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(Model)
	if m.modal.picker != nil || !strings.Contains(m.modal.value("text"), `@"`+spec+`"`) {
		t.Fatalf("the chosen file is inserted: %q", m.modal.value("text"))
	}
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("submit: %s", m.modal.err)
	}
	n, _ := st.TakeNudge("mock-r")
	cp := filepath.Join(refs.Dir(st.Home, "mock-r"), "FPTPAY spec.pdf")
	if n == nil || !strings.Contains(n.Message, "Tệp đính kèm") || !strings.Contains(n.Message, cp) {
		t.Fatalf("nudge = %+v", n)
	}
	if b, err := os.ReadFile(cp); err != nil || string(b) != "%PDF spec" {
		t.Fatal("the file must be copied next to the task")
	}
}

func TestMissingReferenceKeepsTheForm(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-q", Title: "x", Project: "bom"}
	tk.Transition(store.StateFailed, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "m")
	m = typeText(m, "xem @/khong/co/file.pdf")
	m, _ = press(t, m, "ctrl+s")
	if m.modal == nil || !strings.Contains(m.modal.err, "không tìm thấy") {
		t.Fatal("a missing file must be reported before anything is sent")
	}
	if n, _ := st.TakeNudge("mock-q"); n != nil {
		t.Fatal("nothing sent")
	}
}
