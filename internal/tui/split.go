package tui

import (
	"encoding/json"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

// The width of the left panel (lists) against the right one (details) is
// adjustable: < and > move the divider, = puts it back, and the divider can be
// dragged with the mouse. The choice is kept in <home>/tui.json.

const (
	minListWidth   = 24
	minDetailWidth = 30
	splitStep      = 4
)

// uiPrefs is what the TUI remembers between sessions.
type uiPrefs struct {
	// ListPermille is the left panel's share of the screen width, in
	// thousandths (0 = default).
	ListPermille int `json:"list_permille,omitempty"`
}

func (m Model) prefsPath() string {
	if m.d.Store == nil || m.d.Store.Home == "" {
		return ""
	}
	return filepath.Join(m.d.Store.Home, "tui.json")
}

func (m *Model) loadPrefs() {
	if p := m.prefsPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &m.prefs)
		}
	}
}

func (m Model) savePrefs() {
	if p := m.prefsPath(); p != "" {
		if b, err := json.Marshal(m.prefs); err == nil {
			_ = os.WriteFile(p, b, 0o644)
		}
	}
}

// listWidth is the inner width of the left panel.
func (m Model) listWidth() int {
	w := max(34, m.width*2/5)
	if p := m.prefs.ListPermille; p > 0 {
		w = (m.width*p + 500) / 1000
	}
	return min(max(minListWidth, w), max(minListWidth, m.width-minDetailWidth-6))
}

// setListWidth moves the divider so the left panel's inner width is w.
func (m *Model) setListWidth(w int) {
	if m.width <= 0 {
		return
	}
	w = min(max(minListWidth, w), max(minListWidth, m.width-minDetailWidth-6))
	m.prefs.ListPermille = max(1, (w*1000+m.width/2)/m.width)
	m.resize()
}

// splitKey handles < > =; ok=false lets the key through.
func (m Model) splitKey(k string) (Model, bool) {
	switch k {
	case "<":
		m.setListWidth(m.listWidth() - splitStep)
	case ">":
		m.setListWidth(m.listWidth() + splitStep)
	case "=":
		m.prefs.ListPermille = 0
		m.resize()
	default:
		return m, false
	}
	m.savePrefs()
	m.msg = "độ rộng panel: < hẹp lại · > rộng ra · = mặc định · hoặc kéo đường chia bằng chuột"
	return m, true
}

// onDividerDrag drags the divider between the panels; handled=false lets the
// event through. The divider is the left panel's right border and the right
// panel's left border.
func (m Model) onDividerDrag(ev tea.MouseMsg) (Model, bool) {
	if m.tab == tabStatus {
		return m, false
	}
	edge := m.listWidth() + 2 // the left box: border + inner width (with padding) + border
	switch {
	case ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft && ev.Y > 0 &&
		(ev.X == edge-1 || ev.X == edge):
		m.dragging = true
		return m, true
	case m.dragging && ev.Action == tea.MouseActionMotion:
		m.setListWidth(ev.X - 1)
		return m, true
	case m.dragging && ev.Action == tea.MouseActionRelease:
		m.dragging = false
		m.savePrefs()
		return m, true
	}
	return m, false
}
