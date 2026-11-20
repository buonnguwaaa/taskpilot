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
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/herdr"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

type tasksTab struct {
	list   []*store.Task
	cursor int
	// filter: the groups shown (key f); postponed: the tasks on hold (key z).
	filter    listFilter
	postponed map[string]bool
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
	// answer holds the choices made in the detail pane for the pending
	// request's questions (questions.go).
	answer *inlineAnswer
	// workDrafting / workDrafts: the name and result of a Hasaki Work report
	// being drafted by Claude, and the finished drafts (worklog.go).
	workDrafting map[string]bool
	workDrafts   map[string]*workDraftMsg
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
	// focus: the keys scroll the detail pane (Enter/→ in, Esc/← out).
	focus bool
	// view of the detail pane: viewOverview or viewChat (key t).
	view int
	// assess caches the agents' assessments for the list (reloadTasks).
	assess map[string]*store.Assessment
	// assessing: a Claude estimate is running; assessTried: tasks already
	// estimated (or failed) in this session.
	assessing   bool
	assessTried map[string]bool

	// drafting: a prompt is being turned into a ticket draft by Claude.
	drafting bool
	// draft is a finished draft not shown yet (a modal was open when it arrived).
	draft *draftMsg
}

const (
	viewOverview = iota
	viewChat
)

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
	m.tasks.postponed, err = m.d.Store.PostponedIDs()
	if err != nil {
		m.msg = "đọc task tạm hoãn: " + err.Error()
	}
	m.tasks.needsYou = 0
	var groups [len(groupNames)][]*store.Task
	for _, t := range all {
		if m.tasks.waitsOnYou(t) {
			m.tasks.needsYou++
		}
		if !m.tasks.shows(t) || !matchTask(t, m.tasks.query) {
			continue
		}
		g := m.tasks.group(t)
		groups[g] = append(groups[g], t)
	}
	m.tasks.list = m.tasks.list[:0]
	for _, g := range groups {
		m.tasks.list = append(m.tasks.list, g...)
	}
	m.tasks.assess = map[string]*store.Assessment{}
	for _, t := range m.tasks.list {
		if a, _ := m.d.Store.TaskAssessment(t.ID); a != nil {
			m.tasks.assess[t.ID] = a
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
	y, bottom := tt.vp.YOffset, tt.vp.AtBottom()
	tt.vp.SetContent(body)
	switch {
	case changed && tt.view == viewChat:
		tt.vp.GotoBottom() // the latest of the conversation
	case changed:
		tt.vp.GotoTop()
	case tt.view == viewChat && bottom:
		tt.vp.GotoBottom() // follow the conversation as it grows
	default:
		tt.vp.SetYOffset(y)
	}
}

func (m *Model) taskDetail(t *store.Task) string {
	w := max(20, m.tasks.vp.Width)
	tt := &m.tasks
	var b strings.Builder
	reqs, _ := m.d.Store.Requests(t.ID)
	decs, _ := m.d.Store.Decisions(t.ID)
	fmt.Fprintf(&b, "%s\n\n", workflowBar(workflowSteps(t, reqs, decs, tt.pending, tt.prs), w))
	fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(clr(cBright)).Render(wrap(t.Title, w)))
	chips := []string{stateChip(t.State)}
	if tt.group(t) == groupPostponed {
		l := lookOf(t.State)
		chips[0] = fg(cMuted).Render(l.icon + " " + l.label)
	}
	for _, c := range []string{t.Project, "⎇ " + t.Branch, t.TicketID, t.ID} {
		if strings.TrimSpace(strings.TrimPrefix(c, "⎇ ")) != "" && (c != t.ID || t.ID != t.TicketID) {
			chips = append(chips, c)
		}
	}
	fmt.Fprintf(&b, "%s\n\n", truncateANSI(strings.Join(chips, dimStyle.Render(" · ")), w))
	assess := strings.TrimPrefix(m.assessmentLine(t, w-len(kvIndent)), "Level ")
	fmt.Fprintf(&b, "%s\n", kv("Đánh giá", strings.ReplaceAll(assess, "\n", "\n"+kvIndent)))
	fmt.Fprintf(&b, "%s\n", kv("Model", strings.ReplaceAll(dimStyle.Render(wrap(m.profileLine(t), w-len(kvIndent))), "\n", "\n"+kvIndent)))
	fmt.Fprintf(&b, "%s\n", kv("Chi phí", fmt.Sprintf("$%.2f", t.CostUSD)+dimStyle.Render(fmt.Sprintf(" · %d lượt chạy", t.Runs))))
	tabs := []string{" Tổng quan ", " Trò chuyện "}
	for i := range tabs {
		if i == tt.view {
			tabs[i] = activeTabStyle.Render(tabs[i])
		} else {
			tabs[i] = tabStyle.Render(tabs[i])
		}
	}
	scroll := "Enter/→ cuộn khung này"
	if tt.focus {
		scroll = "j/k cuộn · g/G đầu/cuối · Esc/← về danh sách"
	}
	fmt.Fprintf(&b, "\n%s %s\n", strings.Join(tabs, " "), dimStyle.Render(" t đổi · "+scroll))
	fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Foreground(clr(cRule)).Render(strings.Repeat("─", w)))
	if n := m.undoNotice(t); n != "" {
		fmt.Fprintf(&b, "%s\n", quote(accentStyle.Render(wrap(n, w-2)), cRule))
	}
	if tt.view == viewChat {
		if tt.pending != nil {
			fmt.Fprintf(&b, "%s\n", quote(warnStyle.Render(wrap("Đang chờ bạn: "+tt.pending.ID+" (t về Tổng quan để trả lời, hoặc a/x/m)", w-2)), cAccent))
		}
		b.WriteString(m.conversation(t, w))
		return b.String()
	}
	if l := tt.hwLink; l != nil {
		fmt.Fprintf(&b, "%s\n", okMark+" "+accentStyle.Render(fmt.Sprintf("Đã đẩy lên Hasaki Work: %s (id %d) lúc %s", l.Code, l.HWID, l.CreatedAt.Local().Format("02/01 15:04"))))
	}
	if gs, _ := m.d.Store.Grants(t.ID, t.Project); len(gs) > 0 {
		var rs []string
		for _, g := range gs {
			rs = append(rs, g.Rule+map[string]string{store.GrantTask: " (task)", store.GrantProject: " (project)"}[g.Scope])
		}
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(wrap("Quyền đã cấp thêm: "+strings.Join(rs, ", ")+"  · thu hồi: taskpilot grants rm", w)))
	}
	if tks, _ := m.d.Store.TaskTickets(t.ID); len(tks) > 0 {
		for _, x := range tks {
			fmt.Fprintf(&b, "%s\n", truncateANSI(accentStyle.Render("Gộp thêm ticket "+x.TicketID)+dimStyle.Render(" · "+x.Title+" · "+x.At.Local().Format("02/01 15:04")), w))
		}
	}
	if tt.pending != nil {
		p := tt.pending
		var ans *inlineAnswer
		if tt.answer != nil && tt.answer.reqID == p.ID {
			ans = tt.answer
		}
		var c strings.Builder
		switch {
		case isQuestion(p):
			fmt.Fprintf(&b, "\n%s%s\n", warnStyle.Render("● "), section("Agent hỏi bạn ("+p.ID+")", "", w-2))
			fmt.Fprintf(&c, "%s\n", accentStyle.Render(wrap(inlineKeys(p), w-2)))
			if len(p.Questions) > 1 || p.Summary != p.Questions[0].Question {
				fmt.Fprintf(&c, "%s\n", mdWrap(p.Summary, w-2))
			}
			c.WriteString("\n" + inlineQuestions(p, ans, w-2))
		default:
			keys := "a duyệt · A duyệt+ghi chú · x từ chối · m góp ý"
			if len(p.Questions) > 0 {
				keys = inlineKeys(p)
			}
			fmt.Fprintf(&b, "\n%s%s\n", warnStyle.Render("● "), section("Chờ bạn duyệt: "+p.ID, "", w-2))
			fmt.Fprintf(&c, "%s\n%s\n", accentStyle.Render(wrap(keys, w-2)), mdWrap(p.Summary, w-2))
			// The questions come before the long details, so they are in view.
			if len(p.Questions) > 0 {
				c.WriteString("\n" + warnStyle.Render("Câu hỏi kèm theo:") + "\n" + inlineQuestions(p, ans, w-2))
			}
			if p.Details != "" {
				fmt.Fprintf(&c, "\n%s\n", mdWrap(p.Details, w-2))
			}
		}
		fmt.Fprintf(&b, "%s\n", quote(strings.TrimRight(c.String(), "\n"), cAccent))
	}
	postponed, _ := m.d.Store.Postponed(t.ID)
	switch {
	case postponed != nil && !isDone(t):
		// One quiet notice: on hold is not "waiting on you".
		fmt.Fprintf(&b, "\n%s\n", quote(dimStyle.Render("Tạm hoãn từ "+postponed.At.Local().Format("02/01 15:04")+" · ")+
			keyCap("z")+dimStyle.Render(" làm tiếp · ")+keyCap("m")+dimStyle.Render(" nhắn agent · ")+keyCap("o")+dimStyle.Render(" tự làm"), cRule))
	case t.State == store.StateNeedsAttention || t.State == store.StateFailed:
		fmt.Fprintf(&b, "\n%s\n", quote(warnStyle.Render("Task đang dừng: ")+"bấm "+keyCap("m")+" để nhắn agent chạy tiếp, hoặc "+keyCap("o")+" để tự làm", cAccent))
	}
	if len(tt.prs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", section("Pull requests", "p mở", w))
		for _, pr := range sortedPRs(tt.prs) {
			if pr.Pending {
				fmt.Fprintf(&b, "%s\n", wrap(warnMark+" "+pr.Repo+" → "+pr.Destination+dimStyle.Render("  chưa tạo: bấm p để mở trang tạo PR (tiêu đề + mô tả đã copy sẵn)"), w))
				continue
			}
			num := fmt.Sprintf("#%d", pr.ID)
			if pr.URL != "" {
				num = hyperlink(lipgloss.NewStyle().Underline(true).Foreground(clr(cBright)).Render(num), pr.URL)
			}
			line := fmt.Sprintf("%s %s %s %s %s", okMark, titleStyle.Render(pr.Repo), num, dimStyle.Render("→"), pr.Destination)
			if pr.Title != "" {
				line += dimStyle.Render("  " + ansi.Truncate(pr.Title, max(0, w-lipgloss.Width(line)-2), "…"))
			}
			fmt.Fprintf(&b, "%s\n", line)
		}
	}
	if tt.completion != nil {
		fmt.Fprintf(&b, "\n%s\n%s\n", section("Hoàn thành", "", w), quote(mdWrap(tt.completion.Summary, w-2), cOk))
	}
	if t.LastError != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", section("Lỗi", "", w), quote(errStyle.Render(wrap(t.LastError, w-2)), cErr))
	}
	if t.LastResult != "" && tt.pending == nil {
		fmt.Fprintf(&b, "\n%s\n%s\n", section("Tin nhắn cuối của agent", "", w), quote(mdWrap(t.LastResult, w-2), cRule))
	}
	if acts := activity(m.d.Store.EventsPath(t.ID), 60); len(acts) > 0 {
		if tt.showActivity {
			fmt.Fprintf(&b, "\n%s\n", section("▾ Hoạt động", "v thu gọn", w))
			for _, a := range acts {
				fmt.Fprintf(&b, "%s\n", truncate(a, w))
			}
		} else {
			fmt.Fprintf(&b, "\n%s\n", section("▸ Hoạt động", fmt.Sprintf("%d sự kiện · v mở rộng", len(acts)), w))
		}
	}
	if t.Workspace != "" {
		fmt.Fprintf(&b, "\n%s\n", dimStyle.Render("workspace "+t.Workspace+"  (w mở trong IDE · W xem trong repo của bạn)"))
	}
	return b.String()
}

// truncateANSI cuts styled text to w columns.
func truncateANSI(s string, w int) string { return ansi.Truncate(s, w, "…") }

func (m Model) tasksView() string {
	h := m.innerHeight()
	rows := m.listRows()
	lw := m.listWidth()
	cw := lw - 2 // inside the box's padding
	var lines []string
	if len(m.tasks.list) == 0 {
		empty := "(trống)"
		if m.tasks.query != "" {
			empty = "(không có task nào khớp)"
		}
		lines = append(lines, dimStyle.Render(empty))
	}
	all := m.tasks.rows()
	for _, ln := range m.tasks.listLayout(all, rows) {
		r := all[ln.row]
		if r.task < 0 {
			head := fg(cSub).Render(r.header)
			if rest := cw - lipgloss.Width(head) - 1; rest > 0 {
				head += " " + lipgloss.NewStyle().Foreground(clr(cRule)).Render(strings.Repeat("─", rest))
			}
			lines = append(lines, head)
			continue
		}
		lines = append(lines, m.taskEntry(m.tasks.list[r.task], r.task == m.tasks.cursor, cw)[ln.sub])
	}
	title := "Tasks  (/ tìm · f lọc)"
	if f := m.tasks.filter; f != filterAll {
		title = fmt.Sprintf("Tasks: %s (%d)  (f lọc)", filterNames[f], len(m.tasks.list))
	}
	switch {
	case m.tasks.searching:
		title = m.tasks.search.View()
	case m.tasks.query != "":
		title = fmt.Sprintf("Tìm %q: %d task  (/ sửa · Esc xoá)", m.tasks.search.Value(), len(m.tasks.list))
	}
	if vp := m.tasks.vp; vp.TotalLineCount() > vp.Height {
		title += fmt.Sprintf(" · chi tiết %d%%", int(vp.ScrollPercent()*100))
	}
	// Hairline borders; the pane that takes the keys is one step brighter.
	lbox, rbox := boxStyle.BorderForeground(clr(cSub)), boxStyle.BorderForeground(clr(cRule))
	if m.tasks.focus {
		lbox, rbox = rbox, lbox
	}
	left := lbox.Width(lw).Height(h).Render(fit(dimStyle.Render(title)+"\n"+strings.Join(lines, "\n"), lw-2, h))
	right := rbox.Width(m.tasks.vp.Width + 2).Height(h).Render(fit(m.tasks.vp.View(), m.tasks.vp.Width, h))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) tasksKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := m.tasks.current()
	if m.tasks.focus {
		vp := &m.tasks.vp
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
		case "esc", "left", "enter":
			m.tasks.focus = false
			m.refreshTaskDetail(true)
			return m, nil
		}
	}
	switch k.String() {
	case "enter", "right", "l":
		if t != nil {
			m.tasks.focus = true
			m.refreshTaskDetail(true)
		}
		return m, nil
	case "t":
		m.tasks.view = 1 - m.tasks.view
		m.tasks.detailID = "" // scroll as for a new task: top, or the end of the conversation
		m.refreshTaskDetail(true)
		return m, nil
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
	case "H":
		if t == nil || t.Source != hwSource {
			m.msg = "H tạo subtask cho task lấy từ Hasaki Work; task này tạo trong taskpilot: dùng h"
			break
		}
		return m.subtaskFromLocal(t.TicketID)
	case "d", "delete":
		return m.deleteTask(t)
	case "c":
		return m.markDone(t)
	case "e":
		return m.editProfile(t)
	case "w":
		return m.openWorkspace(t)
	case "W":
		return m.reviewInClone(t)
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
	case "f", "F":
		step := map[string]listFilter{"f": 1, "F": filterCount - 1}[k.String()]
		m.tasks.filter = (m.tasks.filter + step) % filterCount
		m.reloadTasks()
		m.msg = "lọc: " + filterNames[m.tasks.filter] + " (f tiếp · F lùi)"
	case "z":
		return m.togglePostpone(t)
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
		if len(req.Questions) > 0 {
			return m.answerForm(t, req, true)
		}
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
		if len(req.Questions) > 0 && (approve || !isQuestion(req)) {
			return m.answerForm(t, req, approve)
		}
		if isQuestion(req) { // x on a question: reply without picking
			return m.replyForm(t, req)
		}
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
		if t != nil && m.tasks.pending != nil { // waiting for you: reply to the request
			return m.replyForm(t, m.tasks.pending)
		}
		if t == nil || !(t.State == store.StateNeedsAttention || t.State == store.StateFailed || t.State == store.StateDone) {
			m.msg = "chỉ nhắn được khi task đang dừng hoặc đang chờ bạn (needs_attention, failed, done, awaiting_approval)"
			break
		}
		return m.messageForm(t, "", "Agent chạy tiếp trong cùng session với tin nhắn này. Gõ nhầm: u để thu hồi (kể cả khi agent đã nhận).")
	case "u":
		return m.undoSend(t)
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
				return infoMsg("đã mở session của " + id + " ở pane " + pane + "; thoát claude để trả task cho daemon")
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
// after, if given, runs once the task was submitted (with its task id).
func (m Model) newTaskForm(init source.Ticket, hint string, after ...func(taskID string)) (tea.Model, tea.Cmd) {
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
		title := strings.TrimSpace(v["title"])
		if title == "" {
			title = source.TitleFrom(v["body"])
		}
		if title == "" {
			return nil, errors.New("nhập tiêu đề hoặc mô tả (câu hỏi)")
		}
		if err := inbox.Submit(source.Ticket{ID: id, Title: title, Body: body, Project: v["project"], Repo: v["repo"]}); err != nil {
			return nil, err
		}
		for _, a := range after {
			a(store.TaskID(inbox.Name(), id))
		}
		return nil, nil
	})
	f.hint = "Agent chỉ làm trong các repo của project đã chọn. Daemon nhận task trong vài giây."
	if hint != "" {
		f.hint = hint + "\n" + f.hint
	}
	f.addSelect("project", "Project", m.projectOptions, init.Project)
	f.addSelect("repo", "Repo bắt đầu", m.repoOptions, init.Repo)
	f.addText("title", "Tiêu đề (bỏ trống: lấy dòng đầu của mô tả)", init.Title, false)
	f.addArea("body", "Mô tả / acceptance criteria — hoặc chỉ một câu hỏi", init.Body, false)
	m.modal = f
	return m, f.start()
}

// attach copies the files referenced in text (@path, Ctrl+O) next to the task
// and appends their list, so the agent reads them whatever their origin.
func (m Model) attach(taskID, text string) (string, error) {
	out, _, err := refs.Attach(text, refs.Dir(m.d.Store.Home, taskID))
	return out, err
}
