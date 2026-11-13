package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestPanelSplit(t *testing.T) {
	m, st, _ := newTest(t)
	def := m.listWidth()
	m, _ = press(t, m, ">")
	if m.listWidth() != def+splitStep || m.tasks.vp.Width != m.width-m.listWidth()-6 {
		t.Fatalf("> widens the list: %d → %d (detail %d)", def, m.listWidth(), m.tasks.vp.Width)
	}
	m, _ = press(t, m, "<", "<")
	if m.listWidth() != def-splitStep {
		t.Fatalf("< narrows: %d", m.listWidth())
	}
	// Bounded on both sides.
	for i := 0; i < 50; i++ {
		m, _ = press(t, m, "<")
	}
	if m.listWidth() != minListWidth {
		t.Fatalf("min = %d", m.listWidth())
	}
	for i := 0; i < 80; i++ {
		m, _ = press(t, m, ">")
	}
	if m.tasks.vp.Width < minDetailWidth {
		t.Fatalf("the detail keeps %d columns, has %d", minDetailWidth, m.tasks.vp.Width)
	}
	m, _ = press(t, m, "=")
	if m.listWidth() != def {
		t.Fatal("= resets")
	}

	// Drag the divider with the mouse.
	edge := m.listWidth() + 2
	mm, _ := m.Update(tea.MouseMsg{X: edge, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	m = mm.(Model)
	mm, _ = m.Update(tea.MouseMsg{X: 70, Y: 5, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft})
	m = mm.(Model)
	mm, _ = m.Update(tea.MouseMsg{X: 70, Y: 5, Action: tea.MouseActionRelease})
	m = mm.(Model)
	if w := m.listWidth(); w < 68 || w > 70 {
		t.Fatalf("dragged to 70: list width %d", w)
	}

	// Remembered by the next TUI.
	m2 := New(Deps{Config: m.d.Config, Store: st, HW: m.d.HW})
	m2.width, m2.height = m.width, m.height
	m2.resize()
	if m2.listWidth() != m.listWidth() {
		t.Fatalf("not remembered: %d vs %d", m2.listWidth(), m.listWidth())
	}
}
