package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Selecting text with the mouse, one pane at a time. A terminal selection
// (Shift + drag) takes whole screen rows, so it also takes the list beside
// the detail and the frames. Here the TUI selects itself: drag in a pane and
// the selection stays inside that pane's text; release and it is copied.
//
// Panes (screen columns, content only — no frame, no padding):
//
//	list    [2, lw)                 lw = listWidth()
//	detail  [lw+4, lw+4+detail width)
//	zoom / Status: the whole width

type selection struct {
	on         bool // a drag started in a pane
	dragged    bool // the mouse moved: a selection, not a click
	x0, x1     int  // the pane's columns [x0, x1)
	y0, y1     int  // the pane's rows [y0, y1)
	ax, ay     int  // anchor (where the drag started)
	bx, by     int  // the other end
	copiedRows int
}

// paneAt returns the content rectangle of the pane under (x, y).
func (m Model) paneAt(x, y int) (x0, x1, y0, y1 int, ok bool) {
	top, bottom := 2, 2+m.innerHeight()
	if m.zoom || m.fullWidthTab() {
		top, bottom = 1, 1+m.innerHeight()+2
		if y < top || y >= bottom {
			return 0, 0, 0, 0, false
		}
		return 0, m.width, top, bottom, true
	}
	if y < top || y >= bottom {
		return 0, 0, 0, 0, false
	}
	lw := m.listWidth()
	switch {
	case x >= 2 && x < lw:
		return 2, lw, top, bottom, true
	case x >= lw+4 && x < m.width-2:
		return lw + 4, m.width - 2, top, bottom, true
	}
	return 0, 0, 0, 0, false
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// onSelect handles the mouse for selections; handled=false lets the event
// through (a press is also a click: it is never swallowed).
func (m Model) onSelect(ev tea.MouseMsg) (Model, tea.Cmd, bool) {
	switch {
	case ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft:
		x0, x1, y0, y1, ok := m.paneAt(ev.X, ev.Y)
		m.sel = selection{}
		if ok {
			m.sel = selection{on: true, x0: x0, x1: x1, y0: y0, y1: y1, ax: ev.X, ay: ev.Y, bx: ev.X, by: ev.Y}
		}
		return m, nil, false
	case ev.Action == tea.MouseActionMotion && m.sel.on:
		m.sel.dragged = true
		m.sel.bx, m.sel.by = clamp(ev.X, m.sel.x0, m.sel.x1-1), clamp(ev.Y, m.sel.y0, m.sel.y1-1)
		return m, nil, true
	case ev.Action == tea.MouseActionRelease && m.sel.on:
		m.sel.on = false
		if !m.sel.dragged {
			m.sel = selection{}
			// A click on a link opens it (the terminal does not see the click).
			if lines := strings.Split(m.screen(), "\n"); ev.Y >= 0 && ev.Y < len(lines) {
				if u := linkAt(lines[ev.Y], ev.X); u != "" && m.d.OpenURL != nil {
					if err := m.d.OpenURL(u); err != nil {
						m.msg = "mở link: " + err.Error()
					} else {
						m.msg = "đã mở " + u
					}
					return m, nil, true
				}
			}
			return m, nil, false
		}
		text, rows := m.selectedText()
		m.sel.copiedRows = rows
		if strings.TrimSpace(text) == "" {
			m.sel = selection{}
			return m, nil, true
		}
		if err := m.d.Copy(text); err != nil {
			m.msg = "copy: " + err.Error()
		} else {
			m.msg = fmt.Sprintf("đã copy %d dòng đang chọn (chỉ trong khung này) · y copy cả khung", rows)
		}
		return m, nil, true
	}
	return m, nil, false
}

// ordered returns the selection's start and end, start first.
func (s selection) ordered() (sx, sy, ex, ey int) {
	if s.ay < s.by || s.ay == s.by && s.ax <= s.bx {
		return s.ax, s.ay, s.bx, s.by
	}
	return s.bx, s.by, s.ax, s.ay
}

// rowSpan is the selected columns [a, b) of screen row y.
func (s selection) rowSpan(y int) (a, b int, ok bool) {
	sx, sy, ex, ey := s.ordered()
	if y < sy || y > ey {
		return 0, 0, false
	}
	a, b = s.x0, s.x1
	if y == sy {
		a = sx
	}
	if y == ey {
		b = ex + 1
	}
	return a, b, a < b
}

// screen is the frame as drawn, before overlays.
func (m Model) screen() string {
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), m.bodyView(), m.footerView())
}

// selectedText is the plain text of the selection: per row, only the
// pane's columns, cleaned of what the panel draws around the text.
func (m Model) selectedText() (string, int) {
	lines := strings.Split(m.screen(), "\n")
	var out []string
	sx, sy, _, ey := m.sel.ordered()
	for y := sy; y <= ey && y < len(lines); y++ {
		a, b, ok := m.sel.rowSpan(y)
		if !ok {
			continue
		}
		out = append(out, ansi.Strip(ansi.Cut(lines[y], a, b)))
	}
	return cleanCopy(out, sx > m.sel.x0), len(out)
}

// cleanCopy turns lines as drawn into the text they show:
//   - the block bar the panel draws on the left ("▌ ") is dropped;
//   - the indent every line shares (markdown blocks, code fences) is dropped
//     (the first line is left out of the count when the selection started
//     inside it);
//   - a line cut by the panel's width inside a "string" is joined back.
func cleanCopy(lines []string, partialFirst bool) string {
	for i, l := range lines {
		l = strings.TrimRight(l, " ")
		if t := strings.TrimLeft(l, " "); strings.HasPrefix(t, "▌") {
			l = strings.TrimPrefix(strings.TrimPrefix(t, "▌"), " ")
		}
		lines[i] = l
	}
	indent := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || i == 0 && partialFirst && len(lines) > 1 {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i, l := range lines {
		if indent > 0 && len(l) >= indent && strings.TrimSpace(l[:indent]) == "" {
			lines[i] = l[indent:]
		}
	}
	// Join a line the panel cut in the middle of a quoted string.
	var out []string
	open := false
	for _, l := range lines {
		if open && len(out) > 0 {
			out[len(out)-1] += " " + strings.TrimLeft(l, " ")
		} else {
			out = append(out, l)
		}
		if strings.Count(strings.ReplaceAll(l, `\"`, ""), `"`)%2 == 1 {
			open = !open
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

var selStyleHL = lipgloss.NewStyle().Reverse(true)

// highlight draws the selection over the frame.
func (m Model) highlight(screen string) string {
	if !m.sel.dragged {
		return screen
	}
	lines := strings.Split(screen, "\n")
	_, sy, _, ey := m.sel.ordered()
	for y := sy; y <= ey && y < len(lines); y++ {
		a, b, ok := m.sel.rowSpan(y)
		if !ok {
			continue
		}
		l := lines[y]
		w := ansi.StringWidth(l)
		if a >= w {
			continue
		}
		b = min(b, w)
		lines[y] = ansi.Cut(l, 0, a) + selStyleHL.Render(ansi.Strip(ansi.Cut(l, a, b))) + ansi.Cut(l, b, w)
	}
	return strings.Join(lines, "\n")
}
