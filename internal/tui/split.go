package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
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
	if m.fullWidthTab() {
		return m, false
	}
	edge := m.listWidth() + 2 // the left box: border + inner width (with padding) + border
	switch {
	case ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft && ev.Y > 0 &&
		(ev.X == edge-1 || ev.X == edge):
		m.dragging, m.dragFrom, m.dragX = true, ev.X, ev.X
		return m, true
	case m.dragging && ev.Action == tea.MouseActionMotion:
		// Only the guide moves: laying the panels out again renders their
		// whole content (a long conversation takes a large part of a second),
		// once per mouse move.
		lw := m.listWidth()
		lo, hi := minListWidth, max(minListWidth, m.width-minDetailWidth-6)
		m.dragX = m.dragFrom + clamp(lw+ev.X-m.dragFrom, lo, hi) - lw
		return m, true
	case m.dragging && ev.Action == tea.MouseActionRelease:
		m.dragging = false
		if w := m.listWidth() + m.dragX - m.dragFrom; w != m.listWidth() {
			m.setListWidth(w)
			m.savePrefs()
		}
		return m, true
	}
	return m, false
}

var dragGuideStyle = fg(cSub)

// dragGuide draws where the divider goes while it is dragged.
func (m Model) dragGuide(screen string) string {
	if !m.dragging {
		return screen
	}
	lines := strings.Split(screen, "\n")
	x := m.listWidth() + 1 + m.dragX - m.dragFrom // the left box's right border, moved
	for y := 1; y < len(lines)-1 && y <= m.innerHeight()+2; y++ {
		l := lines[y]
		w := ansi.StringWidth(l)
		if x >= w {
			continue
		}
		lines[y] = ansi.Cut(l, 0, x) + dragGuideStyle.Render("┃") + ansi.Cut(l, x+1, w)
	}
	return strings.Join(lines, "\n")
}

// fit cuts a panel's content to w columns and h rows: a longer line would be
// wrapped by the box, which then grows taller than its neighbour.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			lines[i] = ansi.Truncate(l, max(1, w), "…")
		}
	}
	return strings.Join(lines, "\n")
}
