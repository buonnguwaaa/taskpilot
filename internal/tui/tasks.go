package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/herdr"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

type tasksTab struct {
	list      []*store.Task
	cursor    int
	inboxOnly bool
	needsYou  int

	// search filters the list as you type (key /); query is its folded value.
	search    textinput.Model
	searching bool
	query     string

	detailID   string
	pending    *store.ApprovalRequest
	completion *store.Completion
	prs        []*store.PullRequest
	hwLink     *store.HasakiLink
	// hwPushing marks tasks being created on Hasaki Work (no double submit).
	hwPushing map[string]bool
	// hwEpics caches the current EPICs per project for key h, with the
	// operator's staff id (the assignee).
	hwEpics        map[string]map[hasakiwork.EpicKind]*hasakiwork.Epic
	hwEpicsAt      time.Time
	hwEpicsLoading bool
	hwStaffID      int64
	prCursor       int
	vp             viewport.Model
	lastBody       string

	// showActivity expands the "Hoạt động" section; collapsed by default.
	showActivity bool

	// drafting: a prompt is being turned into a ticket draft by Claude.
	drafting bool
	// draft is a finished draft not shown yet (a modal was open when it arrived).
	draft *draftMsg
}

func needsOperator(s store.State) bool {
	return s == store.StateAwaitApproval || s == store.StateNeedsAttention || s == store.StateFailed
}

func (t *tasksTab) current() *store.Task {
	if t.cursor < 0 || t.cursor >= len(t.list) {
		return nil
	}
	return t.list[t.cursor]
}

func (m *Model) reloadTasks() {
	all, err := m.d.Store.ListTasks()
	if err != nil {
		m.msg = "đọc task: " + err.Error()
		return
	}
	selected := ""
	if cur := m.tasks.current(); cur != nil {
		selected = cur.ID
	}
	m.tasks.list = m.tasks.list[:0]
	m.tasks.needsYou = 0
	var done []*store.Task
	for _, t := range all {
		if needsOperator(t.State) {
			m.tasks.needsYou++
		}
		if m.tasks.inboxOnly && !needsOperator(t.State) || !matchTask(t, m.tasks.query) {
			continue
		}
		if isDone(t) {
			done = append(done, t) // the "Đã xong" group comes last
		} else {
			m.tasks.list = append(m.tasks.list, t)
		}
	}
	m.tasks.list = append(m.tasks.list, done...)
	m.tasks.cursor = min(m.tasks.cursor, max(0, len(m.tasks.list)-1))
	for i, t := range m.tasks.list {
		if t.ID == selected {
			m.tasks.cursor = i
		}
	}
	m.refreshTaskDetail(false)
}

func (m *Model) moveTask(delta int) {
	if n := len(m.tasks.list); n > 0 {
		m.tasks.cursor = min(max(0, m.tasks.cursor+delta), n-1)
		m.refreshTaskDetail(false)
	}
}

// refreshTaskDetail rebuilds the detail pane, keeping the scroll position
// while the same task stays selected.
func (m *Model) refreshTaskDetail(force bool) {
	tt := &m.tasks
	t := tt.current()
	if t == nil {
		tt.detailID, tt.pending, tt.completion, tt.prs, tt.hwLink = "", nil, nil, nil, nil
		tt.vp.SetContent(dimStyle.Render("Chưa có task. Giao task từ tab 2 Hasaki Work, bấm n để tạo task mới, hoặc N để mô tả bằng lời và để Claude soạn task."))
		return
	}
	changed := t.ID != tt.detailID
	if changed {
		tt.prCursor = 0
	}
	tt.detailID = t.ID
	tt.pending, _ = m.d.Store.PendingRequest(t.ID)
	tt.completion, _ = m.d.Store.Completion(t.ID)
	tt.prs, _ = m.d.Store.PullRequests(t.ID)
	tt.hwLink, _ = m.d.Store.HasakiLink(t.ID)
	body := m.taskDetail(t)
	if body == tt.lastBody && !changed && !force {
		return
	}
	tt.lastBody = body
	y := tt.vp.YOffset
	tt.vp.SetContent(body)
	if changed {
		tt.vp.GotoTop()
	} else {
		tt.vp.SetYOffset(y)
	}
}

func (m *Model) taskDetail(t *store.Task) string {
	w := max(20, m.tasks.vp.Width)
	tt := &m.tasks
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", titleStyle.Render(wrap(t.Title, w)))
	fmt.Fprintf(&b, "%s  project %s  ·  nhánh %s\n", stateBadge(t.State), t.Project, t.Branch)
	fmt.Fprintf(&b, "%s\n", dimStyle.Render(fmt.Sprintf("ticket %s · %d lượt chạy · $%.2f", t.TicketID, t.Runs, t.CostUSD)))
	fmt.Fprintf(&b, "%s\n", dimStyle.Render(wrap(m.profileLine(t), w)))
	if l := tt.hwLink; l != nil {
		fmt.Fprintf(&b, "%s\n", accentStyle.Render(fmt.Sprintf("✓ Đã đẩy lên Hasaki Work: %s (id %d) lúc %s", l.Code, l.HWID, l.CreatedAt.Local().Format("02/01 15:04"))))
	}
	if tt.pending != nil {
		fmt.Fprintf(&b, "\n%s\n%s\n", warnStyle.Render("▶ Chờ bạn duyệt: "+tt.pending.ID+"   (a duyệt · A duyệt+ghi chú · x từ chối)"), wrap(tt.pending.Summary, w))
		if tt.pending.Details != "" {
			fmt.Fprintf(&b, "\n%s\n", wrap(tt.pending.Details, w))
		}
	}
	switch t.State {
	case store.StateNeedsAttention, store.StateFailed:
		fmt.Fprintf(&b, "\n%s\n", warnStyle.Render("▶ Task đang dừng: bấm m để nhắn agent chạy tiếp, hoặc o để tự làm"))
	}
	if len(tt.prs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", titleStyle.Render("Pull requests  (p mở)"))
		for _, pr := range sortedPRs(tt.prs) {
			if pr.Pending {
				fmt.Fprintf(&b, "%s → %s  chưa tạo: bấm p để mở trang tạo PR (tiêu đề + mô tả đã copy sẵn)\n", pr.Repo, pr.Destination)
			} else {
				fmt.Fprintf(&b, "%s\n", wrap(fmt.Sprintf("%s #%d → %s  %s", pr.Repo, pr.ID, pr.Destination, pr.URL), w))
			}
		}
	}
	if tt.completion != nil {
		fmt.Fprintf(&b, "\n%s\n%s\n", titleStyle.Render("✓ Hoàn thành"), wrap(tt.completion.Summary, w))
	}
	if t.LastError != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", errStyle.Render("Lỗi"), wrap(t.LastError, w))
	}
	if t.LastResult != "" && tt.pending == nil {
		fmt.Fprintf(&b, "\n%s\n%s\n", titleStyle.Render("Tin nhắn cuối của agent"), wrap(t.LastResult, w))
	}
	if acts := activity(m.d.Store.EventsPath(t.ID), 60); len(acts) > 0 {
		if tt.showActivity {
			fmt.Fprintf(&b, "\n%s\n", titleStyle.Render("▾ Hoạt động  (v thu gọn)"))
			for _, a := range acts {
				fmt.Fprintf(&b, "%s\n", truncate(a, w))
			}
		} else {
			fmt.Fprintf(&b, "\n%s %s\n", titleStyle.Render("▸ Hoạt động"), dimStyle.Render(fmt.Sprintf("(%d sự kiện · v mở rộng)", len(acts))))
		}
	}
	if t.Workspace != "" {
		fmt.Fprintf(&b, "\n%s\n", dimStyle.Render("workspace "+t.Workspace))
	}
	return b.String()
}

func (m Model) tasksView() string {
	h := m.innerHeight()
	rows := m.listRows()
	lw := m.listWidth()
	var lines []string
	if len(m.tasks.list) == 0 {
		empty := "(trống)"
		if m.tasks.query != "" {
			empty = "(không có task nào khớp)"
		}
		lines = append(lines, dimStyle.Render(empty))
	}
	all := m.tasks.rows()
	for _, r := range all[m.tasks.rowOffset(all, rows):] {
		if len(lines) >= rows {
			break
		}
		if r.task < 0 {
			lines = append(lines, accentStyle.Render("── "+r.header))
			continue
		}
		t := m.tasks.list[r.task]
		text := truncate(t.ID+"  "+t.Title, lw-20)
		if r.task == m.tasks.cursor {
			lines = append(lines, selStyle.Render(fmt.Sprintf("%-17s %s", t.State, text)))
		} else {
			lines = append(lines, fmt.Sprintf("%-17s %s", stateBadge(t.State), text))
		}
	}
	title := "Tasks  (/ tìm)"
	if m.tasks.inboxOnly {
		title = "Tasks cần bạn (f: hiện tất cả)"
	}
	switch {
	case m.tasks.searching:
		title = m.tasks.search.View()
	case m.tasks.query != "":
		title = fmt.Sprintf("Tìm %q: %d task  (/ sửa · Esc xoá)", m.tasks.search.Value(), len(m.tasks.list))
	}
	left := boxStyle.Width(lw).Height(h).Render(dimStyle.Render(title) + "\n" + strings.Join(lines, "\n"))
	right := boxStyle.Width(m.tasks.vp.Width + 2).Height(h).Render(m.tasks.vp.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) tasksKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.tasks.current()
	switch k.String() {
	case "j", "down":
		m.moveTask(1)
	case "k", "up":
		m.moveTask(-1)
	case "home", "g":
		m.moveTask(-len(m.tasks.list))
	case "end", "G":
		m.moveTask(len(m.tasks.list))
	case "pgdown", "ctrl+d", " ":
		m.tasks.vp.HalfViewDown()
	case "pgup", "ctrl+u":
		m.tasks.vp.HalfViewUp()
	case "h":
		return m.pushToHW(t)
	case "d", "delete":
		return m.deleteTask(t)
	case "c":
		return m.markDone(t)
	case "e":
		return m.editProfile(t)
	case "v":
		m.tasks.showActivity = !m.tasks.showActivity
		m.refreshTaskDetail(true)
	case "/":
		return m.startSearch()
	case "esc":
		if m.tasks.query != "" {
			m.tasks.search.SetValue("")
			m.tasks.query = ""
			m.reloadTasks()
		}
	case "f":
		m.tasks.inboxOnly = !m.tasks.inboxOnly
		m.reloadTasks()
	case "r":
		m.reloadTasks()
		m.msg = "đã tải lại"
	case "n":
		return m.newTaskForm(source.Ticket{}, "")
	case "N":
		return m.promptTaskForm("", "")
	case "a":
		if t == nil || m.tasks.pending == nil {
			m.msg = "task này không có yêu cầu nào đang chờ duyệt"
			break
		}
		req := m.tasks.pending
		if err := m.d.Store.Decide(t.ID, req, true, ""); err != nil {
			m.msg = "duyệt: " + err.Error()
		} else {
			m.msg = "đã duyệt " + req.ID
		}
		m.reloadTasks()
	case "A", "x":
		if t == nil || m.tasks.pending == nil {
			m.msg = "task này không có yêu cầu nào đang chờ duyệt"
			break
		}
		approve := k.String() == "A"
		req, id := m.tasks.pending, t.ID
		verb := map[bool]string{true: "Duyệt", false: "Từ chối"}[approve]
		f := newForm(fmt.Sprintf("%s %s — %s", verb, req.ID, truncate(t.Title, 60)), func(v map[string]string) (tea.Cmd, error) {
			comment, err := m.attach(id, v["comment"])
			if err != nil {
				return nil, err
			}
			if err := m.d.Store.Decide(id, req, approve, comment); err != nil {
				return nil, err
			}
			return nil, nil
		})
		f.hint = truncate(req.Summary, 200)
		f.addArea("comment", map[bool]string{true: "Ghi chú cho agent (không bắt buộc)", false: "Cần sửa gì? (agent sẽ đọc nhận xét này)"}[approve], "", !approve)
		m.modal = f
		m.msg = strings.ToLower(verb) + " " + req.ID
		return m, f.start()
	case "m":
		if t == nil || !(t.State == store.StateNeedsAttention || t.State == store.StateFailed || t.State == store.StateDone) {
			m.msg = "chỉ nhắn được khi task đang dừng (needs_attention, failed hoặc done)"
			break
		}
		id := t.ID
		f := newForm("Nhắn agent — "+truncate(t.Title, 60), func(v map[string]string) (tea.Cmd, error) {
			text, err := m.attach(id, v["text"])
			if err != nil {
				return nil, err
			}
			return nil, m.d.Store.SetNudge(id, text)
		})
		f.hint = "Agent chạy tiếp trong cùng session với tin nhắn này."
		f.addArea("text", "Tin nhắn", "", true)
		m.modal = f
		return m, f.start()
	case "o":
		if t == nil {
			break
		}
		if t.State == store.StateRunning || t.State == store.StatePreparing {
			m.msg = "task đang chạy; đợi nó dừng ở cổng rồi mới làm tay"
			break
		}
		if t.Workspace == "" || t.Runs == 0 {
			m.msg = "task chưa có session"
			break
		}
		if m.d.Exe == "" {
			m.msg = "không tìm thấy binary taskpilot"
			break
		}
		argv := []string{m.d.Exe, "takeover", t.ID, "-config", m.d.Config.Path}
		if h := m.d.Herdr; h != nil {
			// Under herdr: open the session in a new pane on the right; the TUI keeps running.
			ws, id := t.Workspace, t.ID
			m.msg = "đang mở session của " + id + " trong pane herdr bên phải…"
			return m, func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				pane, err := h.SplitRight(ctx, ws)
				if err == nil {
					err = h.Run(ctx, pane, herdr.Command(argv...))
				}
				if err != nil {
					return infoMsg("mở pane herdr: " + err.Error())
				}
				return infoMsg("đã mở session của " + id + " ở pane " + pane + " — thoát claude để trả task cho daemon")
			}
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		id := t.ID
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{takeover: id, err: err} })
	case "p":
		if t == nil || len(m.tasks.prs) == 0 {
			m.msg = "task này chưa có PR"
			break
		}
		list := sortedPRs(m.tasks.prs)
		pr := list[m.tasks.prCursor%len(list)]
		m.tasks.prCursor++
		if pr.Pending {
			if err := m.d.Copy(pr.Title + "\n\n" + pr.Description); err != nil {
				m.msg = "copy: " + err.Error()
				break
			}
		}
		if err := m.d.OpenURL(pr.URL); err != nil {
			m.msg = "mở trình duyệt: " + err.Error()
			break
		}
		if pr.Pending {
			m.msg = fmt.Sprintf("đã mở trang tạo PR %s → %s và copy tiêu đề + mô tả: dán vào rồi bấm Create", pr.Repo, pr.Destination)
		} else {
			m.msg = fmt.Sprintf("đã mở PR #%d (%s → %s)", pr.ID, pr.Repo, pr.Destination)
		}
	}
	return m, nil
}

// projectOptions lists configured projects.
func (m Model) projectOptions(*form) []option {
	var out []option
	for _, name := range m.d.Config.ProjectNames() {
		out = append(out, option{Label: m.d.Config.Projects[name].Name, Value: name})
	}
	return out
}

// repoOptions lists the repos of the project chosen in the form.
func (m Model) repoOptions(f *form) []option {
	out := []option{{Label: "agent tự chọn", Value: ""}}
	if p, ok := m.d.Config.Projects[f.value("project")]; ok {
		for _, r := range p.RepoNames() {
			out = append(out, option{Label: r, Value: r})
		}
	}
	return out
}

// newTaskForm opens the task form, prefilled from init (a prompt draft or empty).
func (m Model) newTaskForm(init source.Ticket, hint string) (tea.Model, tea.Cmd) {
	if m.d.Inbox == nil {
		m.msg = "không có inbox để tạo task"
		return m, nil
	}
	inbox := m.d.Inbox
	f := newForm("Task mới", func(v map[string]string) (tea.Cmd, error) {
		if v["project"] == "" {
			return nil, errors.New("chọn project")
		}
		id := time.Now().Format("20060102-150405")
		body, err := m.attach(store.TaskID(inbox.Name(), id), v["body"])
		if err != nil {
			return nil, err
		}
		return nil, inbox.Submit(source.Ticket{ID: id, Title: v["title"], Body: body, Project: v["project"], Repo: v["repo"]})
	})
	f.hint = "Agent chỉ làm trong các repo của project đã chọn. Daemon nhận task trong vài giây."
	if hint != "" {
		f.hint = hint + "\n" + f.hint
	}
	f.addSelect("project", "Project", m.projectOptions, init.Project)
	f.addSelect("repo", "Repo bắt đầu", m.repoOptions, init.Repo)
	f.addText("title", "Tiêu đề", init.Title, true)
	f.addArea("body", "Mô tả / acceptance criteria", init.Body, true)
	m.modal = f
	return m, f.start()
}

// attach copies the files referenced in text (@path, Ctrl+O) next to the task
// and appends their list, so the agent reads them whatever their origin.
func (m Model) attach(taskID, text string) (string, error) {
	out, _, err := refs.Attach(text, refs.Dir(m.d.Store.Home, taskID))
	return out, err
}
