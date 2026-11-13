package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// The Logs tab shows QC (Argo CD) and Production (Splunk) with one layout and
// one set of keys:
//
//	left  : environment · project · time window · actions · the environment's own list
//	right : results, or the error groups
//	keys  : ←/→ project · ↑↓ list · Enter run · g groups · n investigate · / search
//	        t/T time · r rerun · o open in the web UI · c copy · esc back
//
// Only the source differs: saved searches and "ask Claude" for Splunk, the
// applications (health, sync, Enter = pod logs, S sync) for Argo CD.

// logRow is one row of the left list.
type logRow struct {
	label string // already styled when styled is true
	plain string // unstyled text (selection, width)
	note  string // shown under the list for the selected row
	item  int    // index in the environment's items; -1 for a section title
}

// logsLeft renders the left box: header lines, then rows with the cursor.
func (m Model) logsLeft(projects []string, active int, rows []logRow, cursor int, dimCursor bool, extra string) string {
	h := m.innerHeight()
	lw := m.listWidth()
	var b strings.Builder
	label := func(s string) string { return dimStyle.Render(padRight(s, 11)) }
	b.WriteString(label("Môi trường") + m.envLine() + "\n")
	var tabs []string
	for i, p := range projects {
		if i == active {
			tabs = append(tabs, activeTabStyle.Render(" "+p+" "))
		} else {
			tabs = append(tabs, tabStyle.Render(" "+p+" "))
		}
	}
	b.WriteString(label("Dự án") + strings.Join(tabs, " ") + dimStyle.Render("  ←/→") + "\n")
	b.WriteString(label("Thời gian") + warnStyle.Render(" ◷ "+m.presetLabel()) + dimStyle.Render("  t/T đổi") + "\n")
	b.WriteString(ruleStyle.Render(strings.Repeat("─", max(4, lw-2))) + "\n")
	visible := m.listRows() - logsHeaderRows - 4
	start := m.logsScroll(rows, cursor, visible)
	note := ""
	cw := lw - 2
	for i := start; i < len(rows) && i-start < visible; i++ {
		r := rows[i]
		switch {
		case r.item < 0 && strings.HasPrefix(r.plain, "── "):
			// A section title: bold, the rest of the line filled.
			t := truncate(strings.TrimPrefix(r.plain, "── "), cw-2)
			fill := max(0, cw-1-lipgloss.Width(t))
			b.WriteString(accentStyle.Bold(true).Render(t) + " " + ruleStyle.Render(strings.Repeat("─", fill)) + "\n")
		case r.item < 0:
			b.WriteString(dimStyle.Render(truncate(r.plain, cw)) + "\n")
		case r.item == cursor && !dimCursor:
			sel := lipgloss.NewStyle().Background(lipgloss.Color("237"))
			text := truncate(r.plain, cw-2)
			b.WriteString(sel.Foreground(lipgloss.Color("12")).Render("▌ ") +
				sel.Bold(true).Foreground(lipgloss.Color("15")).Render(text+strings.Repeat(" ", max(0, cw-2-lipgloss.Width(text)))) + "\n")
			note = r.note
		case r.item == cursor:
			b.WriteString(accentStyle.Render("› "+truncate(r.plain, cw-2)) + "\n")
		case r.label != "":
			b.WriteString("  " + r.label + "\n")
		default:
			b.WriteString("  " + truncate(r.plain, cw-2) + "\n")
		}
	}
	if note != "" {
		b.WriteString("\n" + quote(dimStyle.Render(wrap(note, cw-2)), "238") + "\n")
	}
	if extra != "" {
		b.WriteString("\n" + extra)
	}
	left := boxStyle.Width(lw).Height(h).Render(b.String())
	right := boxStyle.Width(m.logs.vp.Width + 2).Height(h).Render(m.logs.vp.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// logsHeaderRows are the lines above the list in the left box.
const logsHeaderRows = 4

// logsScroll is the first row shown so the cursor's row is visible.
func (m Model) logsScroll(rows []logRow, cursor, visible int) int {
	pos := 0
	for i, r := range rows {
		if r.item == cursor {
			pos = i
		}
	}
	if pos >= visible {
		return pos - visible + 1
	}
	return 0
}

// logsRowAt maps a click on screen row y to an item (-1: none).
func (m Model) logsRowAt(rows []logRow, cursor, y int) int {
	visible := m.listRows() - logsHeaderRows - 4
	// header line, box border, then the header rows of the box
	i := y - 2 - logsHeaderRows + m.logsScroll(rows, cursor, visible)
	if i < 0 || i >= len(rows) {
		return -1
	}
	return rows[i].item
}

// ---- time window (shared) --------------------------------------------------------

// presetLabel names the window: index 0 is each source's default (a saved
// search's own window on Splunk, the latest lines on Argo CD).
func (m Model) presetLabel() string {
	if m.logs.preset == 0 {
		if m.logs.env == envQC {
			return "mới nhất"
		}
		return "theo truy vấn"
	}
	return splunk.Presets[m.logs.preset-1].Label
}

// qcWindow returns the window of an Argo CD read: seconds back from now
// (0 = the latest lines) and an end before now (zero = now).
func (m Model) qcRange() (since int, until time.Time) {
	if m.logs.preset == 0 {
		return 0, time.Time{}
	}
	p := splunk.Presets[m.logs.preset-1]
	since, until, _ = qcRangeFor(p.Earliest, p.Latest)
	return since, until
}

// ---- error groups (shared) -------------------------------------------------------

// renderGroupList draws error groups the same way for both environments,
// the selected one detailed below; it returns the line of the selection.
// groupKeys is the key line of an error group list.
const groupKeys = "↑↓ chọn · Enter xem log · n điều tra · f sửa ngay · s chỉ service này · esc về danh sách"

func renderGroupList(b *strings.Builder, title string, gs []splunk.ErrorGroup, cursor int, focus bool, w int, keys string) int {
	total := 0
	for _, g := range gs {
		total += g.Count
	}
	b.WriteString(section(title, "", w) + "\n")
	b.WriteString(titleStyle.Render(fmt.Sprint(len(gs))) + dimStyle.Render(" nhóm · ") +
		errStyle.Bold(true).Render(fmt.Sprint(total)) + dimStyle.Render(" lỗi") + "\n")
	b.WriteString(dimStyle.Render(wrap(keys, w)) + "\n\n")
	if len(gs) == 0 {
		b.WriteString(quote(lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("✓ Không có lỗi nào trong khoảng thời gian này."), "10"))
		return 0
	}
	const svcW = 22
	b.WriteString(dimStyle.Render(fmt.Sprintf("  %6s  %-*s %-5s  %s", "SỐ LẦN", svcW, "SERVICE", "CUỐI", "LỖI")) + "\n")
	sel := 0
	for i, g := range gs {
		last := "     "
		if !g.Last.IsZero() {
			last = g.Last.Local().Format("15:04")
		}
		selected := i == cursor && focus
		var bg lipgloss.TerminalColor = lipgloss.NoColor{}
		if selected {
			bg = lipgloss.Color("237")
		}
		st := func(c string) lipgloss.Style {
			s := lipgloss.NewStyle().Background(bg)
			if c != "" {
				s = s.Foreground(lipgloss.Color(c))
			}
			return s
		}
		bar := st("").Render("  ")
		if selected {
			sel = strings.Count(b.String(), "\n")
			bar = st("12").Render("▌ ")
		}
		sig := truncate(strings.TrimSpace(g.Signature), max(1, w-2-6-2-svcW-1-5-2))
		row := bar + st(countColor(g.Count)).Bold(true).Render(fmt.Sprintf("%6d", g.Count)) + st("").Render("  ") +
			st("14").Render(fmt.Sprintf("%-*s", svcW, truncate(g.Service, svcW))) + st("").Render(" ") +
			st("8").Render(last) + st("").Render("  ") + st(map[bool]string{true: "15", false: "7"}[selected]).Render(sig)
		if n := w - lipgloss.Width(row); n > 0 {
			row += st("").Render(strings.Repeat(" ", n))
		}
		b.WriteString(row + "\n")
	}
	if cursor >= 0 && cursor < len(gs) {
		b.WriteString("\n")
		groupDetail(b, gs[cursor], w)
	}
	return sel
}

// countColor grades how often an error happened.
func countColor(n int) string {
	switch {
	case n >= 100:
		return "9"
	case n >= 10:
		return "208"
	}
	return "11"
}

// groupDetail draws one error group: when, how often, a sample, its fields
// and the start of its stacktrace.
func groupDetail(b *strings.Builder, g splunk.ErrorGroup, w int) {
	span := ""
	if !g.First.IsZero() {
		span = fmt.Sprintf(" · %s → %s", g.First.Local().Format("02/01 15:04"), g.Last.Local().Format("02/01 15:04"))
	}
	b.WriteString(section(g.Service, fmt.Sprintf("%d lần%s", g.Count, span), w) + "\n")
	b.WriteString(quote(errStyle.Render(wrap(g.Sample, w-2)), "9") + "\n")
	for _, f := range [][2]string{{"caller", g.Caller}, {"prefix", g.Prefix}, {"request_id", g.RequestID}} {
		if f[1] != "" {
			b.WriteString(kv(f[0], f[1]) + "\n")
		}
	}
	if st := strings.TrimSpace(g.Stacktrace); st != "" {
		lines := strings.Split(st, "\n")
		b.WriteString(dimStyle.Render("stacktrace") + "\n" + quote(dimStyle.Render(strings.Join(lines[:min(6, len(lines))], "\n")), "238") + "\n")
	}
}

// renderResultHeader is the header of a list of log lines, the same for both
// environments.
func renderResultHeader(b *strings.Builder, title, window string, n int, took time.Duration, limit bool, keys string, w int) {
	b.WriteString(section("Kết quả", "", w) + "\n")
	b.WriteString(quote(accentStyle.Render(wrap(title, w-2)), "12") + "\n")
	info := titleStyle.Render(fmt.Sprint(n)) + dimStyle.Render(" dòng")
	if window != "" {
		info = warnStyle.Render("◷ "+window) + dimStyle.Render(" · ") + info
	}
	if took > 0 {
		info += dimStyle.Render(fmt.Sprintf(" · %.1fs", took.Seconds()))
	}
	if limit {
		info += " " + warnStyle.Render("⚠ đã đạt giới hạn: thu hẹp tìm kiếm")
	}
	b.WriteString(info + "\n" + dimStyle.Render(wrap(keys, w)) + "\n\n")
	if n == 0 {
		b.WriteString(dimStyle.Render("Không có kết quả."))
	}
}

// logLineRe splits a formatted log line: "01-02 15:04:05 LEVEL service message".
var logLineRe = regexp.MustCompile(`^(\d\d-\d\d \d\d:\d\d:\d\d) ([A-Z]+ *) (\S+ *) (.*)$`)

var levelColors256 = map[string]string{"ERROR": "9", "FATAL": "9", "PANIC": "9", "DPANI": "9", "DPANIC": "9", "WARN": "208", "WARNING": "208", "INFO": "12", "DEBUG": "8"}

// colorLogLine colors a formatted log line: the time dim, the level by its
// color, the service, the message red on errors.
func colorLogLine(l string) string {
	p := logLineRe.FindStringSubmatch(l)
	if p == nil {
		switch {
		case strings.Contains(l, " ERROR ") || strings.Contains(l, " FATAL ") || strings.Contains(l, " PANIC ") || strings.Contains(l, " DPANI"):
			return errStyle.Render(l)
		case strings.Contains(l, " WARN "):
			return warnStyle.Render(l)
		}
		return l
	}
	level := strings.TrimSpace(p[2])
	c := levelColors256[level]
	if c == "" {
		c = "7"
	}
	msg := p[4]
	switch c {
	case "9":
		msg = errStyle.Render(msg)
	case "208":
		msg = lipgloss.NewStyle().Foreground(lipgloss.Color("223")).Render(msg)
	}
	return dimStyle.Render(p[1]) + " " + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c)).Render(p[2]) + " " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render(p[3]) + " " + msg
}

// logsIntro is the help shown before the first search: a line, then keys.
func logsIntro(lead string, keys [][2]string, w int) string {
	var b strings.Builder
	b.WriteString(section("Bắt đầu", "", w) + "\n" + wrap(lead, w) + "\n\n")
	for _, k := range keys {
		cap := keyCap(k[0])
		b.WriteString(cap + " " + strings.ReplaceAll(dimStyle.Render(wrap(k[1], w-lipgloss.Width(cap)-1)), "\n", "\n"+strings.Repeat(" ", lipgloss.Width(cap)+1)) + "\n")
	}
	return b.String()
}
