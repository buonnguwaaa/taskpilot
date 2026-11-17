package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func mouse(m Model, x, y int, a tea.MouseAction) Model {
	b := tea.MouseButtonLeft
	if a == tea.MouseActionRelease {
		b = tea.MouseButtonNone
	}
	mm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: a, Button: b})
	return mm.(Model)
}

func TestSelectionStaysInItsPane(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-s", Title: "Sửa refund VNPay", Project: "bom",
		LastResult: "dòng một của kết quả\ndòng hai của kết quả\ndòng ba của kết quả"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	m.reloadTasks()
	var copied string
	m.d.Copy = func(s string) error { copied = s; return nil }

	// Find the result lines on screen, in the detail pane.
	lines := strings.Split(m.screen(), "\n")
	row := -1
	for y, l := range lines {
		if strings.Contains(ansi.Strip(l), "dòng một của kết quả") {
			row = y
		}
	}
	if row < 0 {
		t.Fatalf("no result line:\n%s", m.View())
	}
	x0 := m.listWidth() + 4
	// Drag from the start of "dòng một" to the end of "dòng hai", overshooting
	// into nothing on the right and starting left of the pane's text.
	m = mouse(m, x0, row, tea.MouseActionPress)
	m = mouse(m, m.width+20, row+1, tea.MouseActionMotion)
	if !strings.Contains(m.View(), "\x1b[7m") && !m.sel.dragged {
		t.Fatal("the selection is highlighted")
	}
	m = mouse(m, m.width+20, row+1, tea.MouseActionRelease)
	got := strings.Split(copied, "\n")
	if len(got) != 2 || !strings.HasSuffix(got[0], "dòng một của kết quả") || !strings.HasSuffix(got[1], "dòng hai của kết quả") {
		t.Fatalf("copied %q", copied)
	}
	if strings.Contains(copied, "│") || strings.Contains(copied, "mock-s") {
		t.Fatal("no frame, no list")
	}
	// A plain click is a click, not a selection.
	copied = ""
	m = mouse(m, x0, row, tea.MouseActionPress)
	m = mouse(m, x0, row, tea.MouseActionRelease)
	if copied != "" || m.sel.dragged {
		t.Fatal("a click copies nothing")
	}
	// In the list, the selection stays in the list.
	m = mouse(m, 2, 3, tea.MouseActionPress)
	m = mouse(m, x0+30, 3, tea.MouseActionMotion)
	m = mouse(m, x0+30, 3, tea.MouseActionRelease)
	if strings.Contains(copied, "dòng") || copied == "" {
		t.Fatalf("list selection: %q", copied)
	}
}

func TestCleanCopy(t *testing.T) {
	in := []string{
		"{",
		"▌     \"customer_id\": \"1024587\",",
		"▌     \"user_agent\": \"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36",
		"▌     (KHTML, like Gecko) Chrome/114.0.0.0 Mobile Safari/537.36\",",
		"▌     \"installment_term\": 12   ",
		"▌   }",
	}
	want := "{\n" +
		"  \"customer_id\": \"1024587\",\n" +
		"  \"user_agent\": \"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Mobile Safari/537.36\",\n" +
		"  \"installment_term\": 12\n" +
		"}"
	if got := cleanCopy(in, true); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := cleanCopy([]string{"▌ a", "▌   b"}, false); got != "a\n  b" {
		t.Fatalf("%q", got)
	}
}
