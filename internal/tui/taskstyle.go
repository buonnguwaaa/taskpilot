package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// The look of the Tasks tab: task states as an icon and a Vietnamese label,
// two-line list entries, and detail sections with a heading rule.

type stateLook struct{ icon, label, color string }

var stateLooks = map[store.State]stateLook{
	store.StateQueued:         {"◷", "chờ chạy", "12"},
	store.StatePreparing:      {"◌", "chuẩn bị", "12"},
	store.StateRunning:        {"▶", "đang chạy", "14"},
	store.StateAwaitApproval:  {"⏸", "chờ bạn duyệt", "11"},
	store.StateNeedsAttention: {"⚠", "cần bạn", "208"},
	store.StateDone:           {"✓", "xong", "10"},
	store.StateFailed:         {"✗", "lỗi", "9"},
}

func lookOf(s store.State) stateLook {
	if l, ok := stateLooks[s]; ok {
		return l
	}
	return stateLook{"•", string(s), "7"}
}

// stateChip is the state as a colored pill, e.g. " ⚠ cần bạn ".
func stateChip(s store.State) string {
	l := lookOf(s)
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color(l.color)).
		Render(" " + l.icon + " " + l.label + " ")
}

// section is a detail heading: the title, a hint, then a rule to the edge.
func section(title, hint string, w int) string {
	head := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(title)
	if hint != "" {
		head += " " + dimStyle.Render(hint)
	}
	if rest := w - lipgloss.Width(head) - 1; rest > 0 {
		head += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Repeat("─", rest))
	}
	return head
}

// quote puts a colored bar in front of every line of s (already wrapped to
// two columns less than the pane).
func quote(s, color string) string {
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("▌") + " "
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = bar + l
	}
	return strings.Join(lines, "\n")
}

// ruleStyle draws rules and section fills.
var ruleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))

// kvIndent lines up a value's continuation lines under it.
const kvIndent = "           "

// kv is one "label  value" line of the detail header.
func kv(label, value string) string {
	return dimStyle.Render(fmt.Sprintf("%-10s", label)) + " " + value
}

// link is label opening url in the terminal.
func hyperlink(label, url string) string {
	return osc8(fmt.Sprintf("l%x", fnvHash(url)), url) + label + osc8("", "")
}

func fnvHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

// taskEntryLines is how many lines a task takes in the list.
const taskEntryLines = 2

// listLine is one printed line of the task list: rows[row], its sub-th line.
type listLine struct{ row, sub int }

// listLayout lays the task list out on h lines: headers take one line, tasks
// two; the cursor's task stays in view.
func (t *tasksTab) listLayout(rows []listRow, h int) []listLine {
	var lines []listLine
	last := -1
	for i, r := range rows {
		n := 1
		if r.task >= 0 {
			n = taskEntryLines
		}
		for k := range n {
			lines = append(lines, listLine{i, k})
		}
		if r.task >= 0 && r.task == t.cursor {
			last = len(lines) - 1
		}
	}
	off := max(0, last-h+1)
	return lines[off:min(len(lines), off+h)]
}

// part is a colored piece of an entry's second line ("" color: dim).
type part struct{ text, color string }

// entryLines renders a two-line list entry w columns wide: the icon and the
// title, then the parts joined by " · "; selected entries get a bar and a
// background.
func entryLines(selected, faded bool, w int, icon, iconColor, title string, parts []part) [2]string {
	var bg lipgloss.TerminalColor = lipgloss.NoColor{}
	if selected {
		bg = lipgloss.Color("237")
	}
	st := func(color string) lipgloss.Style {
		s := lipgloss.NewStyle().Background(bg)
		if color != "" {
			s = s.Foreground(lipgloss.Color(color))
		}
		return s
	}
	bar := st("").Render("  ")
	if selected {
		bar = st("12").Render("▌ ")
	}
	tc := map[bool]string{true: "15", false: "7"}[selected]
	if faded && !selected {
		tc = "8"
	}
	line1 := bar + st(iconColor).Render(icon+" ") + st(tc).Bold(selected).Render(ansi.Truncate(title, max(1, w-4), "…"))

	line2, used := bar+st("").Render("  "), 4
	for i, p := range parts {
		if i > 0 {
			line2 += st("8").Render(" · ")
			used += 3
		}
		c := p.color
		if c == "" {
			c = "8"
		}
		text := p.text
		if i == len(parts)-1 {
			text = ansi.Truncate(text, max(1, w-used), "…")
		}
		line2 += st(c).Render(text)
		used += ansi.StringWidth(text)
	}
	pad := func(s string) string {
		if n := w - lipgloss.Width(s); n > 0 {
			s += st("").Render(strings.Repeat(" ", n))
		}
		return s
	}
	return [2]string{pad(line1), pad(line2)}
}

// taskEntry renders the lines of one task in the list, w columns wide.
func (m Model) taskEntry(t *store.Task, selected bool, w int) [2]string {
	l := lookOf(t.State)
	parts := []part{{l.label, l.color}}
	if a := m.tasks.assess[t.ID]; a != nil && levelLabels[a.Level] != "" {
		parts = append(parts, part{"● " + levelLabels[a.Level], levelColors[a.Level]})
	}
	meta := t.ID
	if t.Project != "" {
		meta = t.Project + " · " + meta
	}
	parts = append(parts, part{meta, ""})
	return entryLines(selected, isDone(t), w, l.icon, l.color, t.Title, parts)
}

// entryOffset is the first entry shown so the cursor's entry stays in view
// when each entry takes taskEntryLines of the rows lines.
func entryOffset(cursor, rows int) int {
	return max(0, cursor-max(1, rows/taskEntryLines)+1)
}
