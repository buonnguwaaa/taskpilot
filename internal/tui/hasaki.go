package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
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
	content  string // the detail pane's text (copy, zoom)
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
	// focus sends the movement keys to the detail pane (scroll) instead of the list.
	focus bool
}

func (h *hwTab) current() *hasakiwork.Task {
	if h.cursor < 0 || h.cursor >= len(h.list) {
		return nil
	}
	return &h.list[h.cursor]
}

// offset is the first entry shown in rows lines (two lines an entry).
func (h *hwTab) offset(rows int) int { return entryOffset(h.cursor, rows) }

// hwLooks are the icon, label and color of the Hasaki Work task statuses.
var hwLooks = map[int]stateLook{
	hasakiwork.StatusNew:             {"○", "mới", cSub},
	hasakiwork.StatusProcessing:      {"▸", "đang làm", cBright},
	hasakiwork.StatusFinished:        {"✓", "xong", cOk},
	hasakiwork.StatusLate:            {"!", "trễ hạn", cErr},
	hasakiwork.StatusCancelled:       {"✗", "đã huỷ", cMuted},
	hasakiwork.StatusWaitingApproval: {"◌", "chờ duyệt", cSub},
	hasakiwork.StatusFailed:          {"✗", "không đạt", cErr},
}

func hwLook(status int) stateLook {
	if l, ok := hwLooks[status]; ok {
		return l
	}
	return stateLook{"•", hasakiwork.StatusName(status), cSub}
}

// hwOpen reports whether a task still has work to do (a deadline matters).
func hwOpen(status int) bool {
	return status == hasakiwork.StatusNew || status == hasakiwork.StatusProcessing || status == hasakiwork.StatusLate
}

// hwDeadline is the deadline as "hạn 30/11" with its color: red once past,
// orange within two days ("" when there is none).
func hwDeadline(t *hasakiwork.Task, now time.Time) part {
	end, err := time.ParseInLocation("2006-01-02 15:04:05", t.DateEnd, time.Local)
	if err != nil {
		if end, err = time.ParseInLocation("2006-01-02", t.DateEnd, time.Local); err != nil {
			return part{}
		}
	}
	text := "hạn " + end.Format("02/01")
	if end.Year() != now.Year() {
		text = "hạn " + end.Format("02/01/2006")
	}
	switch left := end.Sub(now); {
	case !hwOpen(t.Status):
		return part{text, ""}
	case left < 0:
		return part{"quá " + text, cErr}
	case left < 48*time.Hour:
		return part{text, cAccent}
	}
	return part{text, cSub}
}

// hwEntry renders one task of the list.
func (m Model) hwEntry(t *hasakiwork.Task, selected bool, w int) [2]string {
	l := hwLook(t.Status)
	icon, color := l.icon, l.color
	parts := []part{{l.label, l.color}}
	if m.dispatched(t) {
		icon, color = "◆", cSub
		parts = append(parts, part{"đã giao agent", cSub})
	}
	if d := hwDeadline(t, time.Now()); d.text != "" {
		parts = append(parts, d)
	}
	scope := m.scope(t)
	if scope == "" && t.Project != nil {
		scope = t.Project.Name
	}
	meta := t.Code
	if scope != "" {
		meta = scope + " · " + meta
	}
	parts = append(parts, part{meta, ""})
	return entryLines(selected, !hwOpen(t.Status), w, icon, color, t.Name, parts)
}

func (m Model) loadHW() (tea.Model, tea.Cmd) {
	if m.d.HW == nil {
		m.hw.err = "Chưa đăng nhập Hasaki Work: vào tab 4 Status và bấm l"
		m.refreshHWDetail()
		return m, nil
	}
	m.hw.loading, m.hw.err = true, ""
	filter := hwFilters[m.hw.filter].value
	hw := m.d.HW
	m.refreshHWDetail()
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
		m.refreshHWDetail()
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
	m.refreshHWDetail()
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
	m.refreshHWDetail()
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

func (m Model) dispatched(t *hasakiwork.Task) bool { return m.hwHost(t.Code) != "" }

func (m *Model) refreshHWDetail() {
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
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(clr(cBright)).Render(wrap(t.Name, w)))
		l := hwLook(t.Status)
		chip := fg(l.color).Render(l.icon + " " + l.label)
		chips := []string{chip, t.Code}
		if t.Project != nil {
			chips = append(chips, t.Project.Name)
		}
		if scope := m.scope(t); scope != "" {
			chips = append(chips, dimStyle.Render("taskpilot: ")+scope)
		}
		fmt.Fprintf(&b, "%s\n\n", truncateANSI(strings.Join(chips, dimStyle.Render(" · ")), w))
		if d := hwDeadline(t, time.Now()); d.text != "" {
			c := d.color
			if c == "" {
				c = cSub
			}
			fmt.Fprintf(&b, "%s\n", kv("Hạn", lipgloss.NewStyle().Foreground(clr(c)).Render(strings.TrimPrefix(strings.TrimPrefix(d.text, "quá "), "hạn "))+
				map[bool]string{true: errStyle.Render("  (đã quá hạn)"), false: ""}[strings.HasPrefix(d.text, "quá ")]))
		}
		if t.Milestone != nil {
			fmt.Fprintf(&b, "%s\n", kv("Sprint", t.Milestone.Name))
		}
		if t.Parent != nil {
			fmt.Fprintf(&b, "%s\n", kv("Thuộc", truncateANSI(t.Parent.Name, w-len(kvIndent))))
		}
		if t.CreatedBy != nil {
			fmt.Fprintf(&b, "%s\n", kv("Người tạo", t.CreatedBy.StaffName))
		}
		if t.PlannedHours > 0 {
			fmt.Fprintf(&b, "%s\n", kv("Dự kiến", fmt.Sprintf("%gh", t.PlannedHours/60)))
		}
		b.WriteString("\n")
		switch scope := m.scope(t); {
		case m.dispatched(t):
			b.WriteString(quote(accentStyle.Render("Đã giao cho agent: task "+m.hwHost(t.Code))+dimStyle.Render(" · Enter mở ở tab 1"), cRule) + "\n")
		case scope == "":
			b.WriteString(quote(warnStyle.Render("Project này chưa có trong config taskpilot nên không giao được."), cAccent) + "\n")
		default:
			b.WriteString(quote(keyCap("Enter")+" giao cho agent "+dimStyle.Render("(phạm vi: "+scope+")")+"   "+keyCap("a")+" gộp vào task đang làm   "+keyCap("n")+" tạo task con", cRule) + "\n")
		}
		b.WriteString("\n" + section("Mô tả", "", w) + "\n")
		if full, ok := h.details[t.ID]; ok {
			if strings.TrimSpace(full.Note) == "" {
				b.WriteString(dimStyle.Render("(task không có mô tả; nên bổ sung khi giao)"))
			} else {
				b.WriteString(mdWrap(full.Note, w))
			}
			if len(full.NoteFiles) > 0 {
				b.WriteString("\n\n" + section(fmt.Sprintf("Tệp đính kèm (%d)", len(full.NoteFiles)), "", w) + "\n")
				for _, p := range full.NoteFiles {
					b.WriteString(dimStyle.Render("• "+hasakiwork.FileName(p)) + "\n")
				}
				b.WriteString(dimStyle.Render("(tải về cạnh task khi giao cho agent)"))
			}
			if links := claudeai.FindLinks(full.Note); len(links) > 0 {
				b.WriteString("\n\n" + section(fmt.Sprintf("Prototype claude.ai (%d)", len(links)), "", w) + "\n")
				for _, l := range links {
					id, _ := claudeai.Slug(l)
					b.WriteString(dimStyle.Render("• "+id) + "\n")
				}
				b.WriteString(dimStyle.Render("(khi giao: mở bằng Chrome riêng với phiên `taskpilot auth claude`, tải mã nguồn + ảnh chụp)"))
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
	h.content = b.String()
	h.vp.SetContent(h.content)
	if id != h.shownID {
		h.vp.GotoTop()
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
	for i := m.hw.offset(rows); i < len(m.hw.list) && len(lines)+taskEntryLines <= rows; i++ {
		e := m.hwEntry(&m.hw.list[i], i == m.hw.cursor, lw-2)
		lines = append(lines, e[:]...)
	}
	if len(lines) == 0 && !m.hw.loading {
		lines = append(lines, dimStyle.Render("(trống)"))
	}
	if vp := m.hw.vp; vp.TotalLineCount() > vp.Height {
		title += fmt.Sprintf(" · chi tiết %d%%", int(vp.ScrollPercent()*100))
	}
	// Hairline borders; the pane that takes the keys is one step brighter (as in Tasks).
	lbox, rbox := boxStyle.BorderForeground(clr(cSub)), boxStyle.BorderForeground(clr(cRule))
	if m.hw.focus {
		lbox, rbox = rbox, lbox
	}
	left := lbox.Width(lw).Height(h).Render(fit(dimStyle.Render(title)+"\n"+strings.Join(lines, "\n"), lw-2, h))
	right := rbox.Width(m.hw.vp.Width + 2).Height(h).Render(fit(m.hw.vp.View(), m.hw.vp.Width, h))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) hwKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.hw.focus {
		vp := &m.hw.vp
		switch k.String() {
		case "j", "down":
			vp.ScrollDown(1)
			return m, nil
		case "k", "up":
			vp.ScrollUp(1)
			return m, nil
		case "g", "home":
			vp.GotoTop()
			return m, nil
		case "G", "end":
			vp.GotoBottom()
			return m, nil
		case "esc", "left", "h":
			m.hw.focus = false
			return m, nil
		}
	}
	switch k.String() {
	case "l", "right":
		if m.hw.current() != nil {
			m.hw.focus = true
		}
		return m, nil
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
	case "a":
		return m.attachHW()
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
	if id := m.hwHost(t.Code); id != "" {
		m.tab = tabTasks
		m.tasks.filter = filterAll
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
	inbox, task, hw, fetchArt := m.d.Inbox, *full, m.d.HW, m.d.ClaudeArtifact
	f := newForm("Giao cho agent: "+t.Code, func(v map[string]string) (tea.Cmd, error) {
		taskID := store.TaskID(hwSource, task.Code)
		notes, err := m.attach(taskID, v["notes"])
		if err != nil {
			return nil, err
		}
		dir := refs.Dir(m.d.Store.Home, taskID)
		tk := source.Ticket{ID: task.Code, Source: hwSource, Title: task.Name, Project: v["project"], Repo: v["repo"]}
		return func() tea.Msg {
			files := hwDownloadFiles(hw, &task, dir)
			files = append(files, fetchArtifacts(fetchArt, task.Note+"\n"+notes, dir)...)
			tk.Body = hwTicketBody(&task, notes, files)
			if err := inbox.Submit(tk); err != nil {
				return infoMsg("giao " + task.Code + " thất bại: " + err.Error())
			}
			msg := "đã giao " + task.Code + " cho agent"
			if n := len(files); n > 0 {
				failed := 0
				for _, f := range files {
					if f.err != nil {
						failed++
					}
				}
				msg += fmt.Sprintf(" (%d tệp đính kèm", n-failed)
				if failed > 0 {
					msg += fmt.Sprintf(", %d tệp không tải được", failed)
				}
				msg += ")"
			}
			return infoMsg(msg)
		}, nil
	})
	f.hint = truncate(t.Name, 120)
	f.addSelect("project", "Phạm vi (project)", m.projectOptions, scope)
	f.addSelect("repo", "Repo bắt đầu", m.repoOptions, "")
	f.addArea("notes", "Ghi chú thêm cho agent (không bắt buộc)", "", false)
	m.modal = f
	m.msg = "giao " + t.Code
	return m, f.start()
}

// hwFile is one file attached to a Hasaki Work task, or one claude.ai
// artifact linked from it, as downloaded for the agent.
type hwFile struct {
	url   string
	local string // empty when the download failed
	err   error
	// artifact is the description of a downloaded claude.ai artifact (its
	// files and a screenshot), instead of local.
	artifact string
}

// fetchArtifacts downloads the claude.ai artifacts (prototypes) linked from
// text into dir.
func fetchArtifacts(fetch func(ctx context.Context, link, dir string) (*claudeai.Artifact, error), text, dir string) []hwFile {
	links := claudeai.FindLinks(text)
	if len(links) == 0 {
		return nil
	}
	var out []hwFile
	for _, l := range links {
		if fetch == nil {
			out = append(out, hwFile{url: l, err: errors.New("taskpilot cannot open claude.ai links here")})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		a, err := fetch(ctx, l, dir)
		cancel()
		if err != nil {
			out = append(out, hwFile{url: l, err: err})
			continue
		}
		out = append(out, hwFile{url: l, artifact: a.Format()})
	}
	return out
}

// hwDownloadFiles saves the files attached to the task's description into
// dir (next to the task, where the agent can read them).
func hwDownloadFiles(hw HW, t *hasakiwork.Task, dir string) []hwFile {
	var out []hwFile
	for _, p := range t.NoteFiles {
		if strings.TrimSpace(p) == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		local, err := hw.DownloadFile(ctx, p, dir)
		cancel()
		out = append(out, hwFile{url: hw.FileURL(p), local: local, err: err})
	}
	return out
}

// hwTicketBody turns a Hasaki Work task into the ticket text the agent reads.
func hwTicketBody(t *hasakiwork.Task, notes string, files []hwFile) string {
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
	if len(files) > 0 {
		b.WriteString("\nFiles attached to or linked from the task (part of the requirement — read every one; images and PDFs with Read, other formats with a tool; a claude.ai prototype comes as its source files plus a screenshot):\n")
		for _, f := range files {
			switch {
			case f.err != nil:
				fmt.Fprintf(&b, "- %s (could not download: %v; ask the operator for it, or call read_claude_artifact for a claude.ai link)\n", f.url, f.err)
			case f.artifact != "":
				b.WriteString("- " + strings.ReplaceAll(strings.TrimRight(f.artifact, "\n"), "\n", "\n  ") + "\n")
			default:
				fmt.Fprintf(&b, "- %s\n", f.local)
			}
		}
	}
	if notes = strings.TrimSpace(notes); notes != "" {
		b.WriteString("\nOperator notes:\n" + notes + "\n")
	}
	return b.String()
}
