package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/herdr"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

type tasksTab struct {
	list      []*store.Task
	cursor    int
	inboxOnly bool
	needsYou  int

	detailID   string
	pending    *store.ApprovalRequest
	completion *store.Completion
	prs        []*store.PullRequest
	prCursor   int
	vp         viewport.Model
	lastBody   string
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

func (t *tasksTab) offset(h int) int {
	if t.cursor >= h {
		return t.cursor - h + 1
	}
	return 0
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
	for _, t := range all {
		if needsOperator(t.State) {
			m.tasks.needsYou++
		}
		if !m.tasks.inboxOnly || needsOperator(t.State) {
			m.tasks.list = append(m.tasks.list, t)
		}
	}
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
		tt.detailID, tt.pending, tt.completion, tt.prs = "", nil, nil, nil
		tt.vp.SetContent(dimStyle.Render("Chưa có task. Giao task từ tab 2 Hasaki Work, hoặc bấm n để tạo task mới."))
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
				fmt.Fprintf(&b, "%s #%d → %s  %s\n", pr.Repo, pr.ID, pr.Destination, pr.URL)
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
		fmt.Fprintf(&b, "\n%s\n", titleStyle.Render("Hoạt động"))
		for _, a := range acts {
			fmt.Fprintf(&b, "%s\n", truncate(a, w))
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
		lines = append(lines, dimStyle.Render("(trống)"))
	}
	for i := m.tasks.offset(rows); i < len(m.tasks.list) && len(lines) < rows; i++ {
		t := m.tasks.list[i]
		text := truncate(t.ID+"  "+t.Title, lw-20)
		if i == m.tasks.cursor {
			lines = append(lines, selStyle.Render(fmt.Sprintf("%-17s %s", t.State, text)))
		} else {
			lines = append(lines, fmt.Sprintf("%-17s %s", stateBadge(t.State), text))
		}
	}
	title := "Tasks"
	if m.tasks.inboxOnly {
		title = "Tasks cần bạn (f: hiện tất cả)"
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
	case "f":
		m.tasks.inboxOnly = !m.tasks.inboxOnly
		m.reloadTasks()
	case "r":
		m.reloadTasks()
		m.msg = "đã tải lại"
	case "n":
		return m.newTaskForm()
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
			if err := m.d.Store.Decide(id, req, approve, v["comment"]); err != nil {
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
			return nil, m.d.Store.SetNudge(id, v["text"])
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

func (m Model) newTaskForm() (tea.Model, tea.Cmd) {
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
		return nil, inbox.Submit(source.Ticket{ID: id, Title: v["title"], Body: v["body"], Project: v["project"], Repo: v["repo"]})
	})
	f.hint = "Agent chỉ làm trong các repo của project đã chọn. Daemon nhận task trong vài giây."
	f.addSelect("project", "Project", m.projectOptions, "")
	f.addSelect("repo", "Repo bắt đầu", m.repoOptions, "")
	f.addText("title", "Tiêu đề", "", true)
	f.addArea("body", "Mô tả / acceptance criteria", "", true)
	m.modal = f
	return m, f.start()
}
