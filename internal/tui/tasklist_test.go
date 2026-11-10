package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func listFixture(t *testing.T) Model {
	t.Helper()
	m, st, _ := newTest(t)
	for _, x := range []struct {
		id, title, project string
		state              store.State
	}{
		{"t1", "Cải tiến import PL schedule", "bom", store.StateDone},
		{"t2", "Thêm API export CSV", "bom", store.StateRunning},
		{"t3", "Sửa lỗi đồng bộ style", "plm", store.StateDone},
		{"t4", "Upgrade kafka", "plm", store.StateNeedsAttention},
	} {
		tk := &store.Task{ID: x.id, Title: x.title, Project: x.project}
		tk.Transition(x.state, "")
		if err := st.SaveTask(tk); err != nil {
			t.Fatal(err)
		}
	}
	m.reloadTasks()
	return m
}

func ids(m Model) string {
	var out []string
	for _, t := range m.tasks.list {
		out = append(out, t.ID)
	}
	return strings.Join(out, ",")
}

func TestTaskGroups(t *testing.T) {
	m := listFixture(t)
	if got := ids(m); got != "t4,t2,t3,t1" {
		t.Fatalf("active tasks first, then done: %s", got)
	}
	v := m.View()
	ia, id := strings.Index(v, "Đang làm (2)"), strings.Index(v, "Đã xong (2)")
	if ia < 0 || id < 0 || ia > id {
		t.Fatalf("group headers missing or out of order:\n%s", v)
	}
	// Headers are skipped by the cursor.
	m, _ = press(t, m, "down", "down")
	if m.tasks.current().ID != "t3" {
		t.Fatalf("cursor on %s", m.tasks.current().ID)
	}
	// Clicking a row maps through the headers: row 0 header, 1 t4, 2 t2, 3 header, 4 t3.
	click := func(row int) string {
		tm, _ := m.Update(tea.MouseMsg{X: 2, Y: listTop + row, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		mm := tm.(Model)
		return mm.tasks.current().ID
	}
	if got := click(1); got != "t4" {
		t.Fatalf("click row 1 → %s", got)
	}
	if got := click(3); got != "t3" {
		t.Fatal("clicking a header must not move the cursor")
	}
}

func TestTaskSearch(t *testing.T) {
	m := listFixture(t)
	m, _ = press(t, m, "/")
	if !m.tasks.searching {
		t.Fatal("/ opens the search box")
	}
	m = typeText(m, "cai tien") // no diacritics
	if got := ids(m); got != "t1" {
		t.Fatalf("search 'cai tien' → %s", got)
	}
	m, _ = press(t, m, "enter")
	if m.tasks.searching || ids(m) != "t1" {
		t.Fatal("enter keeps the filter and leaves the box")
	}
	if !strings.Contains(m.View(), `Tìm "cai tien": 1 task`) {
		t.Fatal("the active filter is shown")
	}
	m, _ = press(t, m, "esc")
	if ids(m) != "t4,t2,t3,t1" {
		t.Fatal("esc clears the filter")
	}

	// Several words: all must match (project + word); q and digits are text, not shortcuts.
	m, _ = press(t, m, "/")
	m = typeText(m, "plm dong bo")
	if got := ids(m); got != "t3" {
		t.Fatalf("search 'plm dong bo' → %s", got)
	}
	m = typeText(m, " q1")
	if m.tab != tabTasks || ids(m) != "" || !strings.Contains(m.View(), "không có task nào khớp") {
		t.Fatalf("typing q/1 must stay in the box: tab=%d ids=%q", m.tab, ids(m))
	}
	m, _ = press(t, m, "esc")
	if m.tasks.searching || ids(m) != "t4,t2,t3,t1" {
		t.Fatal("esc in the box clears and closes it")
	}
}

func TestFold(t *testing.T) {
	if got := fold("Đồng Bộ Cải Tiến"); got != "dong bo cai tien" {
		t.Fatalf("fold = %q", got)
	}
}
