package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/text/unicode/norm"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// The task list is split in three groups, each under a header row:
// "Đang làm" (anything not done nor on hold, failed included), "Tạm hoãn"
// (put on hold by the operator, key z) then "Đã xong". Key f shows one
// group only, or the tasks waiting on you.

// isDone reports whether a task is finished.
func isDone(t *store.Task) bool { return t.State == store.StateDone }

type taskGroup int

const (
	groupActive taskGroup = iota
	groupPostponed
	groupDone
)

var groupNames = [...]string{"Đang làm", "Tạm hoãn", "Đã xong"}

// group is the list group of a task.
func (t *tasksTab) group(x *store.Task) taskGroup {
	switch {
	case isDone(x):
		return groupDone
	case t.postponed[x.ID]:
		return groupPostponed
	}
	return groupActive
}

// listFilter is what the list shows (key f cycles through them).
type listFilter int

const (
	filterAll listFilter = iota
	filterActive
	filterPostponed
	filterDone
	filterInbox
	filterCount
)

var filterNames = [...]string{"Tất cả", "Đang làm", "Tạm hoãn", "Đã xong", "Cần bạn"}

// shows reports whether the filter keeps a task.
func (t *tasksTab) shows(x *store.Task) bool {
	switch t.filter {
	case filterActive:
		return t.group(x) == groupActive
	case filterPostponed:
		return t.group(x) == groupPostponed
	case filterDone:
		return t.group(x) == groupDone
	case filterInbox:
		return t.waitsOnYou(x)
	}
	return true
}

// waitsOnYou: the task is parked on the operator and not put on hold.
func (t *tasksTab) waitsOnYou(x *store.Task) bool {
	return needsOperator(x.State) && !t.postponed[x.ID]
}

// listRow is one row of the task list: a group header (task < 0) or a task.
type listRow struct {
	task   int // index in tasksTab.list; -1 for a header
	header string
}

// rows lays out the list: the groups with their headers, in list order
// (reloadTasks sorts the list by group).
func (t *tasksTab) rows() []listRow {
	var count [len(groupNames)]int
	for _, x := range t.list {
		count[t.group(x)]++
	}
	var out []listRow
	for i, x := range t.list {
		// One group shown (key f): the list title names it, no header.
		if g := t.group(x); t.filter == filterAll && (i == 0 || g != t.group(t.list[i-1])) {
			out = append(out, listRow{task: -1, header: fmt.Sprintf("%s (%d)", groupNames[g], count[g])})
		}
		out = append(out, listRow{task: i})
	}
	return out
}

// rowOffset is the first row shown so the cursor's row stays visible in h
// rows (at the bottom once scrolled, so the group header above it shows too).
func (t *tasksTab) rowOffset(rows []listRow, h int) int {
	for i, r := range rows {
		if t.cursor >= 0 && r.task == t.cursor {
			return max(0, i-h+1)
		}
	}
	return 0
}

// fold lowercases s and strips Vietnamese diacritics, so "cai tien" finds
// "Cải tiến".
func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case r == 'đ':
			r = 'd'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// matchTask reports whether every word of the (folded) query appears in the
// task's id, title, project, branch, ticket or state.
func matchTask(t *store.Task, query string) bool {
	words := strings.Fields(query)
	if len(words) == 0 {
		return true
	}
	hay := fold(strings.Join([]string{t.ID, t.Title, t.Project, t.Branch, t.TicketID, string(t.State)}, " "))
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// startSearch opens the search box above the list.
func (m Model) startSearch() (tea.Model, tea.Cmd) {
	if m.tasks.search.Prompt == "" { // zero value: not created yet
		m.tasks.search = textinput.New()
		m.tasks.search.Prompt = "/ "
		m.tasks.search.Placeholder = "tìm id, tên, project, nhánh…"
		m.tasks.search.CharLimit = 100
	}
	m.tasks.searching = true
	m.tasks.search.Width = max(10, m.listWidth()-4)
	m.msg = "gõ để lọc · Enter giữ bộ lọc · Esc xoá"
	return m, m.tasks.search.Focus()
}

// searchKey handles a key while the search box has the focus.
func (m Model) searchKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.tasks.searching = false
		m.tasks.search.Blur()
		m.tasks.search.SetValue("")
		m.tasks.query = ""
		m.msg = ""
		m.reloadTasks()
		return m, nil
	case "enter":
		m.tasks.searching = false
		m.tasks.search.Blur()
		m.msg = ""
		return m, nil
	case "up", "down":
		m.moveTask(map[string]int{"up": -1, "down": 1}[k.String()])
		return m, nil
	case "ctrl+c":
		return m.quit()
	}
	var cmd tea.Cmd
	m.tasks.search, cmd = m.tasks.search.Update(k)
	if q := fold(strings.TrimSpace(m.tasks.search.Value())); q != m.tasks.query {
		m.tasks.query = q
		m.tasks.cursor = 0
		m.reloadTasks()
	}
	return m, cmd
}
