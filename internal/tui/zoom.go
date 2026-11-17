package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The terminal selects whole screen rows: dragging over the detail pane
// also takes the list beside it and the frames. Two ways out:
//
//	F3  the detail pane alone, full width and without frame (select freely)
//	y   copy the whole detail pane as plain text

// toggleZoom switches the full-width detail pane.
func (m Model) toggleZoom() Model {
	m.zoom = !m.zoom
	m.resize()
	if m.zoom {
		m.msg = "khung chi tiết toàn màn hình: bôi đen thoải mái (Shift + kéo) · y copy cả khung · F3 trở lại"
	} else {
		m.msg = "đã trở lại hai khung"
	}
	return m
}

// detailPane is the detail viewport of the current tab and its full text.
func (m Model) detailPane() (view, text string) {
	switch m.tab {
	case tabTasks:
		return m.tasks.vp.View(), m.tasks.lastBody
	case tabHasaki:
		return m.hw.vp.View(), m.hw.content
	case tabLogs:
		return m.logs.vp.View(), m.logs.vpContent
	case tabStatus:
		return m.stVP.View(), m.stVP.View()
	}
	return "", ""
}

func (m Model) zoomView() string {
	view, _ := m.detailPane()
	return lipgloss.NewStyle().Width(m.width).Height(m.innerHeight() + 2).Render(view)
}

// copyDetail copies the whole detail pane (not only what is on screen) as
// plain text: no colors, no links' escape codes, no frame.
func (m Model) copyDetail() Model {
	_, text := m.detailPane()
	text = strings.TrimSpace(ansi.Strip(text))
	if text == "" {
		m.msg = "khung chi tiết đang trống"
		return m
	}
	lines := strings.Split(text, "\n")
	text = cleanCopy(lines, false)
	if err := m.d.Copy(text); err != nil {
		m.msg = "copy: " + err.Error()
		return m
	}
	m.msg = fmt.Sprintf("đã copy khung chi tiết (%d dòng) vào clipboard", len(lines))
	return m
}

// zoomScroll scrolls the zoomed pane with the wheel.
func (m *Model) zoomScroll(up bool) {
	vp := &m.tasks.vp
	switch m.tab {
	case tabHasaki:
		vp = &m.hw.vp
	case tabLogs:
		vp = &m.logs.vp
	case tabStatus:
		vp = &m.stVP
	}
	if up {
		vp.ScrollUp(3)
	} else {
		vp.ScrollDown(3)
	}
}
