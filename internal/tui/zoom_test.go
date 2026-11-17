package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestZoomAndCopyDetail(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-z", Title: "Sửa refund VNPay", Project: "bom", LastResult: "kết quả: đã sửa ở payment-vnpay/internal/a.go:12"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	var copied string
	m.d.Copy = func(s string) error { copied = s; return nil }

	m, _ = press(t, m, "y")
	if !strings.Contains(copied, "Sửa refund VNPay") || !strings.Contains(copied, "payment-vnpay/internal/a.go:12") ||
		strings.Contains(copied, "\x1b") || strings.Contains(copied, "│") {
		t.Fatalf("plain copy of the detail pane: %q", copied)
	}
	if strings.Contains(copied, "mock-z  Sửa") {
		t.Fatal("the task list is not copied")
	}

	wide := m.tasks.vp.Width
	m, _ = press(t, m, "f3")
	v := m.View()
	if !m.zoom || m.tasks.vp.Width <= wide || strings.Contains(v, "Đã xong (1)") || strings.Contains(v, "│") {
		t.Fatalf("zoom: the detail alone, without frames:\n%s", v)
	}
	if !strings.Contains(v, "Sửa refund VNPay") {
		t.Fatalf("zoomed view:\n%s", v)
	}
	// Clicks do nothing in the zoomed pane (there is no list to click).
	mm, _ := m.Update(tea.MouseMsg{X: 3, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(Model)
	m, _ = press(t, m, "f3")
	if m.zoom || m.tasks.vp.Width != wide || !strings.Contains(m.View(), "Đã xong (1)") {
		t.Fatal("F3 again: back to two panes")
	}
}
