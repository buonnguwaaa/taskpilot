package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// hwSource is the task-id prefix of Hasaki Work tasks dispatched from the TUI.
const hwSource = "hw"

var hwFilters = []struct{ value, label string }{
	{"_00_01", "đang mở"},
	{"_05", "chờ duyệt"},
	{"_02", "đã xong"},
}

type hwTab struct {
	list     []hasakiwork.Task
	epics    int // EPICs left out of list: they group tasks, there is nothing to do on them
	cursor   int
	filter   int
	loading  bool
	loaded   bool
	err      string
	details  map[int64]*hasakiwork.Task
	fetching map[int64]bool
	// dispatchAfterLoad opens the dispatch form once this task's description arrives.
	dispatchAfterLoad int64
	vp                viewport.Model
	shownID           int64
}

func (h *hwTab) current() *hasakiwork.Task {
	if h.cursor < 0 || h.cursor >= len(h.list) {
		return nil
	}
	return &h.list[h.cursor]
}

func (h *hwTab) offset(rows int) int {
	if h.cursor >= rows {
		return h.cursor - rows + 1
	}
	return 0
}

func (m Model) loadHW() (tea.Model, tea.Cmd) {
	if m.d.HW == nil {
		m.hw.err = "Chưa đăng nhập Hasaki Work: vào tab 4 Status và bấm l"
		m.refreshHWDetail(true)
		return m, nil
	}
	m.hw.loading, m.hw.err = true, ""
	filter := hwFilters[m.hw.filter].value
	hw := m.d.HW
	m.refreshHWDetail(true)
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		tasks, err := hw.MyTasks(ctx, filter)
		return hwTasksMsg{filter: filter, tasks: tasks, err: err}
	}
}

func (m Model) onHWTasks(msg hwTasksMsg) (tea.Model, tea.Cmd) {
	if msg.filter != hwFilters[m.hw.filter].value {
		return m, nil // a stale response for another filter
	}
	m.hw.loading, m.hw.loaded = false, true
	if msg.err != nil {
		m.hw.err = msg.err.Error()
		m.refreshHWDetail(true)
		return m, nil
	}
	m.hw.list, m.hw.epics = nil, 0
	for _, t := range msg.tasks {
		if hasakiwork.IsEpic(t.Name) {
			m.hw.epics++
			continue
		}
		m.hw.list = append(m.hw.list, t)
	}
	m.hw.cursor = min(m.hw.cursor, max(0, len(m.hw.list)-1))
	return m.selectHW()
}

// selectHW shows the selected task and fetches its description if needed.
func (m Model) selectHW() (tea.Model, tea.Cmd) {
	m.refreshHWDetail(false)
	t := m.hw.current()
	if t == nil || m.d.HW == nil {
		return m, nil
	}
	if _, ok := m.hw.details[t.ID]; ok || m.hw.fetching[t.ID] {
		return m, nil
	}
	if m.hw.fetching == nil {
		m.hw.fetching = map[int64]bool{}
	}
	m.hw.fetching[t.ID] = true
	id, hw := t.ID, m.d.HW
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		full, err := hw.Task(ctx, id)
		return hwDetailMsg{id: id, task: full, err: err}
	}
}

func (m Model) onHWDetail(msg hwDetailMsg) (tea.Model, tea.Cmd) {
	delete(m.hw.fetching, msg.id)
	if msg.err != nil {
		m.msg = "tải mô tả task: " + msg.err.Error()
		m.hw.dispatchAfterLoad = 0
		return m, nil
	}
	m.hw.details[msg.id] = msg.task
	m.refreshHWDetail(true)
	if m.hw.dispatchAfterLoad == msg.id {
		m.hw.dispatchAfterLoad = 0
		return m.dispatchHW()
	}
	return m, nil
}

func (m Model) moveHW(delta int) (tea.Model, tea.Cmd) {
	if n := len(m.hw.list); n > 0 {
		m.hw.cursor = min(max(0, m.hw.cursor+delta), n-1)
		return m.selectHW()
	}
	return m, nil
}

// scope returns the taskpilot project of a Hasaki Work task.
func (m Model) scope(t *hasakiwork.Task) string {
	name, _ := m.d.Config.ProjectByHasakiID(t.ProjectID)
	return name
}

func (m Model) dispatched(t *hasakiwork.Task) bool {
	return t.Code != "" && m.d.Store.Exists(store.TaskID(hwSource, t.Code))
}

func (m *Model) refreshHWDetail(force bool) {
	h := &m.hw
	w := max(20, h.vp.Width)
	var b strings.Builder
	t := h.current()
	switch {
	case h.err != "":
		b.WriteString(errStyle.Render(wrap(h.err, w)))
	case h.loading && len(h.list) == 0:
		b.WriteString(dimStyle.Render("Đang tải task từ Hasaki Work…"))
	case t == nil:
		b.WriteString(dimStyle.Render("Không có task nào với bộ lọc này (s: đổi bộ lọc)."))
	default:
		fmt.Fprintf(&b, "%s\n", titleStyle.Render(wrap(t.Name, w)))
		proj := ""
		if t.Project != nil {
			proj = t.Project.Name
		}
		fmt.Fprintf(&b, "%s · %s · project %s\n", t.Code, hasakiwork.StatusName(t.Status), proj)
		if t.Milestone != nil {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render("sprint: "+t.Milestone.Name))
		}
		if t.Parent != nil {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render("thuộc: "+t.Parent.Name))
		}
		if t.DateEnd != "" {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render("hạn: "+t.DateEnd))
		}
		if t.CreatedBy != nil {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render("người tạo: "+t.CreatedBy.StaffName))
		}
		b.WriteString("\n")
		switch scope := m.scope(t); {
		case m.dispatched(t):
			b.WriteString(accentStyle.Render("✓ Đã giao cho agent: task "+store.TaskID(hwSource, t.Code)+" (tab 1)") + "\n")
		case scope == "":
			b.WriteString(warnStyle.Render("Project này chưa có trong config taskpilot nên không giao được.") + "\n")
		default:
			b.WriteString(accentStyle.Render(fmt.Sprintf("Enter: giao cho agent (phạm vi: %s)", scope)) + "\n")
		}
		b.WriteString("\n" + titleStyle.Render("Mô tả") + "\n")
		if full, ok := h.details[t.ID]; ok {
			if strings.TrimSpace(full.Note) == "" {
				b.WriteString(dimStyle.Render("(task không có mô tả — nên bổ sung khi giao)"))
			} else {
				b.WriteString(wrap(full.Note, w))
			}
		} else {
			b.WriteString(dimStyle.Render("đang tải…"))
		}
	}
	var id int64
	if t != nil {
		id = t.ID
	}
	y := h.vp.YOffset
	h.vp.SetContent(b.String())
	if id != h.shownID {
		h.vp.GotoTop()
	} else if !force {
		h.vp.SetYOffset(y)
	} else {
		h.vp.SetYOffset(y)
	}
	h.shownID = id
}

func (m Model) hwView() string {
	h := m.innerHeight()
	rows := m.listRows()
	lw := m.listWidth()
	title := fmt.Sprintf("Hasaki Work · %s (s đổi)", hwFilters[m.hw.filter].label)
	if m.hw.loading {
		title += " · đang tải…"
	} else if m.hw.epics > 0 {
		title += fmt.Sprintf(" · ẩn %d EPIC", m.hw.epics)
	}
	var lines []string
	for i := m.hw.offset(rows); i < len(m.hw.list) && len(lines) < rows; i++ {
		t := &m.hw.list[i]
		scope := m.scope(t)
		if scope == "" {
			scope = "-"
		}
		mark := " "
		if m.dispatched(t) {
			mark = "✓"
		}
		text := fmt.Sprintf("%s %-8s %s", mark, scope, truncate(t.Name, lw-14))
		if i == m.hw.cursor {
			lines = append(lines, selStyle.Render(text))
		} else {
			lines = append(lines, text)
		}
	}
	if len(lines) == 0 && !m.hw.loading {
		lines = append(lines, dimStyle.Render("(trống)"))
	}
	left := boxStyle.Width(lw).Height(h).Render(dimStyle.Render(title) + "\n" + strings.Join(lines, "\n"))
	right := boxStyle.Width(m.hw.vp.Width + 2).Height(h).Render(m.hw.vp.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) hwKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "j", "down":
		return m.moveHW(1)
	case "k", "up":
		return m.moveHW(-1)
	case "home", "g":
		return m.moveHW(-len(m.hw.list))
	case "end", "G":
		return m.moveHW(len(m.hw.list))
	case "pgdown", "ctrl+d", " ":
		m.hw.vp.HalfViewDown()
	case "pgup", "ctrl+u":
		m.hw.vp.HalfViewUp()
	case "s":
		m.hw.filter = (m.hw.filter + 1) % len(hwFilters)
		m.hw.list, m.hw.cursor = nil, 0
		return m.loadHW()
	case "r":
		return m.loadHW()
	case "enter":
		return m.dispatchHW()
	case "n":
		t := m.hw.current()
		if t == nil {
			return m, nil
		}
		if full, ok := m.hw.details[t.ID]; ok {
			return m.subtaskForm(*full)
		}
		return m.subtaskForm(*t)
	}
	return m, nil
}

// dispatchHW opens the form that hands the selected Hasaki Work task to an agent.
func (m Model) dispatchHW() (tea.Model, tea.Cmd) {
	t := m.hw.current()
	if t == nil {
		return m, nil
	}
	if m.dispatched(t) {
		id := store.TaskID(hwSource, t.Code)
		m.tab = tabTasks
		m.tasks.inboxOnly = false
		m.reloadTasks()
		for i, x := range m.tasks.list {
			if x.ID == id {
				m.tasks.cursor = i
			}
		}
		m.refreshTaskDetail(false)
		m.msg = "task này đã được giao: " + id
		return m, nil
	}
	scope := m.scope(t)
	if scope == "" {
		m.msg = "project của task chưa có trong config taskpilot"
		return m, nil
	}
	full, ok := m.hw.details[t.ID]
	if !ok {
		m.hw.dispatchAfterLoad = t.ID
		m.msg = "đang tải mô tả task…"
		return m.selectHW()
	}
	if m.d.Inbox == nil {
		m.msg = "không có inbox để giao task"
		return m, nil
	}
	inbox, task := m.d.Inbox, *full
	f := newForm("Giao cho agent: "+t.Code, func(v map[string]string) (tea.Cmd, error) {
		notes, err := m.attach(store.TaskID(hwSource, task.Code), v["notes"])
		if err != nil {
			return nil, err
		}
		body := hwTicketBody(&task, notes)
		return nil, inbox.Submit(source.Ticket{ID: task.Code, Source: hwSource, Title: task.Name, Body: body, Project: v["project"], Repo: v["repo"]})
	})
	f.hint = truncate(t.Name, 120)
	f.addSelect("project", "Phạm vi (project)", m.projectOptions, scope)
	f.addSelect("repo", "Repo bắt đầu", m.repoOptions, "")
	f.addArea("notes", "Ghi chú thêm cho agent (không bắt buộc)", "", false)
	m.modal = f
	m.msg = "giao " + t.Code
	return m, f.start()
}

// hwTicketBody turns a Hasaki Work task into the ticket text the agent reads.
func hwTicketBody(t *hasakiwork.Task, notes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hasaki Work task %s (id %d)", t.Code, t.ID)
	if t.Project != nil {
		fmt.Fprintf(&b, " — project %s", t.Project.Name)
	}
	b.WriteString("\n")
	if t.Milestone != nil {
		fmt.Fprintf(&b, "Sprint: %s\n", t.Milestone.Name)
	}
	if t.Parent != nil {
		fmt.Fprintf(&b, "Parent: %s %s\n", t.Parent.Code, t.Parent.Name)
	}
	if t.DateEnd != "" {
		fmt.Fprintf(&b, "Deadline: %s\n", t.DateEnd)
	}
	if t.CreatedBy != nil {
		fmt.Fprintf(&b, "Requested by: %s\n", t.CreatedBy.StaffName)
	}
	b.WriteString("\nDescription:\n")
	if note := strings.TrimSpace(t.Note); note != "" {
		b.WriteString(note + "\n")
	} else {
		b.WriteString("(no description in Hasaki Work)\n")
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		b.WriteString("\nOperator notes:\n" + notes + "\n")
	}
	return b.String()
}
