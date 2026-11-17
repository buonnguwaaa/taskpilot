// Package tui is the operator console. Everything is reachable with keys or
// the mouse: the task inbox (approve / reject / message / take over / PRs),
// the Hasaki Work task list (pick a task and dispatch it to an agent), and a
// status page (logins, daemon, logs). ":" opens a command line that runs any
// `taskpilot` subcommand for the cases the screens do not cover.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/claudeai"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/desktop"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/intake"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splgen"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

// HW is the part of the Hasaki Work client the TUI uses.
type HW interface {
	MyTasks(ctx context.Context, statusFilter string) ([]hasakiwork.Task, error)
	Task(ctx context.Context, id int64) (*hasakiwork.Task, error)
	// Used by key h in the Tasks tab (create a task on Hasaki Work, report).
	Profile(ctx context.Context) (*hasakiwork.Profile, error)
	CurrentEpics(ctx context.Context, projectID int64, mine []hasakiwork.Task) (map[hasakiwork.EpicKind]*hasakiwork.Epic, error)
	CreateTask(ctx context.Context, t hasakiwork.NewTask) (*hasakiwork.Created, error)
	AssignStaff(ctx context.Context, taskID int64, staffIDs ...int64) error
	Report(ctx context.Context, taskID, staffID int64, result string, realityMinutes, status int) error
	// DownloadFile saves a task file (Task.NoteFiles) into dir.
	DownloadFile(ctx context.Context, path, dir string) (string, error)
	FileURL(path string) string
}

// Herdr is the part of the herdr client the TUI uses.
type Herdr interface {
	SplitRight(ctx context.Context, cwd string) (string, error)
	Run(ctx context.Context, paneID, command string) error
}

// DaemonInfo tells the TUI about the scheduler.
type DaemonInfo struct {
	Embedded bool       // the daemon runs inside this process
	External bool       // another process holds the scheduler lock
	InHerdr  bool       // ... in a herdr tab (survives closing this TUI)
	InFlight func() int // runs in progress (embedded only)
	Logs     func() []string
}

// Deps are the TUI's collaborators. Only Config and Store are required.
type Deps struct {
	Config *config.Config
	Store  *store.Store
	HW     HW           // nil: the Hasaki Work tab explains how to log in
	Inbox  *source.Mock // where tickets created or dispatched from the TUI go
	Exe    string       // taskpilot binary, for the command line
	Daemon DaemonInfo
	// Banner returns a header line (Hasaki Work token expiry) and whether it needs attention.
	Banner func() (string, bool)
	// Herdr, when set (running inside herdr), opens agent sessions in panes.
	Herdr Herdr
	// Splunk returns a client for the production logs (nil: not configured).
	Splunk func(ctx context.Context) (*splunk.Client, error)
	// ArgoCD returns a client for the QC environment (nil: not configured).
	ArgoCD func(ctx context.Context) (*argocd.Client, error)
	// ClaudeArtifact downloads a claude.ai artifact (prototype) linked from a
	// task into dir; nil when not wired.
	ClaudeArtifact func(ctx context.Context, link, dir string) (*claudeai.Artifact, error)
	// AuthStatus returns the lines of the status page's login section.
	AuthStatus func() []string
	OpenURL    func(string) error
	Copy       func(string) error
	// PasteFiles reads an image or files from the clipboard (Alt+V in a form).
	PasteFiles func(ctx context.Context, dir string) ([]string, error)
	// PasteText reads the clipboard's text (Ctrl+V in a form when it holds no image).
	PasteText func(ctx context.Context) (string, error)
	// OpenEditor opens a directory in the operator's IDE (key w).
	OpenEditor func(dir string) error
	// Draft turns a free-form prompt into a ticket draft (nil: "N" is unavailable).
	Draft func(ctx context.Context, prompt string) (*intake.Draft, error)
	// WorkLog drafts the name and result of a Hasaki Work report (nil: the
	// forms are prefilled without Claude).
	WorkLog func(ctx context.Context, in worklog.Input) (*worklog.Draft, error)
	// Assess estimates the level and kind of a task the agent has not
	// assessed (nil: not shown until the agent does).
	Assess func(ctx context.Context, in worklog.AssessInput) (*worklog.Assessment, error)
	// SPLGen writes a Splunk search from a question (nil: "Hỏi bằng lời" is unavailable).
	SPLGen func(ctx context.Context, lc splgen.Context, question string) (*splgen.Result, error)
}

type tabID int

const (
	tabTasks tabID = iota
	tabHasaki
	tabLogs
	tabStatus
)

var tabNames = []string{"Tasks", "Hasaki Work", "Logs", "Status"}

type (
	infoMsg     string
	tickMsg     time.Time
	execDoneMsg struct {
		takeover string
		err      error
	}
	cmdOutputMsg struct {
		title string
		out   string
		err   error
	}
	hwTasksMsg struct {
		filter string
		tasks  []hasakiwork.Task
		err    error
	}
	hwDetailMsg struct {
		id   int64
		task *hasakiwork.Task
		err  error
	}
)

type outputView struct {
	title string
	vp    viewport.Model
}

// Model is the root Bubble Tea model.
type Model struct {
	d        Deps
	cfgWatch *config.Watcher // reloads d.Config when the file changes
	// mouseOff hands the mouse back to the terminal (F2), so text can be
	// selected and copied; clicks and the wheel then do nothing in the TUI.
	mouseOff bool
	zoom     bool      // the detail pane fills the screen (F3, zoom.go)
	sel      selection // a mouse selection inside one pane (selection.go)
	prefs    uiPrefs
	dragging bool // the panel divider is being dragged (split.go)
	dragFrom int  // the column the drag started at
	dragX    int  // where the divider is being dragged to (drawn as a guide)

	width, height int
	tab           tabID
	help          bool
	msg           string

	tasks tasksTab
	hw    hwTab
	logs  logsTab
	stVP  viewport.Model

	modal   *form
	conf    *confirm
	output  *outputView
	palette bool
	cmdline textinput.Model
	history []string
	histPos int

	bannerText string
	bannerWarn bool
}

// New builds the model.
func New(d Deps) Model {
	if d.OpenURL == nil {
		d.OpenURL = desktop.OpenURL
	}
	if d.Copy == nil {
		d.Copy = desktop.Copy
	}
	if d.PasteFiles == nil {
		d.PasteFiles = desktop.PasteFiles
	}
	if d.PasteText == nil {
		d.PasteText = desktop.PasteText
	}
	if d.OpenEditor == nil {
		editor := ""
		if d.Config != nil {
			editor = d.Config.Editor
		}
		d.OpenEditor = func(dir string) error { return openEditor(editor, dir) }
	}
	ci := textinput.New()
	ci.Prompt = ":"
	ci.CharLimit = 2000
	m := Model{d: d, width: 120, height: 40, cmdline: ci}
	m.tasks.vp = viewport.New(60, 20)
	m.hw.vp = viewport.New(60, 20)
	m.hw.details = map[int64]*hasakiwork.Task{}
	m.logs.vp = viewport.New(60, 20)
	m.logs.intro = true
	m.stVP = viewport.New(100, 20)
	m.loadPrefs()
	m.resize()
	m.reloadTasks()
	m.refreshBanner()
	return m
}

// Run starts the TUI full-screen with mouse support.
func Run(d Deps) error {
	_, err := tea.NewProgram(New(d), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return tick() }

func (m *Model) refreshBanner() {
	if m.d.Banner != nil {
		m.bannerText, m.bannerWarn = m.d.Banner()
	}
}

// bodyView is the current tab between the header and the footer.
func (m Model) bodyView() string {
	switch {
	case m.zoom && m.tab != tabStatus:
		return m.zoomView()
	case m.tab == tabTasks:
		return m.tasksView()
	case m.tab == tabHasaki:
		return m.hwView()
	case m.tab == tabLogs:
		return m.logsView()
	case m.tab == tabStatus:
		return lipgloss.NewStyle().Width(m.width).Height(m.stVP.Height).Render(m.stVP.View()) // the cards have their own frames
	}
	return ""
}

// ---- layout ---------------------------------------------------------------

func (m Model) innerHeight() int { return max(3, m.height-4) } // header + footer + box borders

// listRows is how many list entries fit under a list box's title line.
func (m Model) listRows() int { return m.innerHeight() - 1 }

// listTop is the screen row of the first list entry: header, box border, list title.
const listTop = 3

func (m *Model) resize() {
	dw := max(20, m.width-m.listWidth()-6)
	h := m.innerHeight()
	if m.zoom {
		// The detail pane alone, without frame: a terminal selection then
		// takes only its text (zoom.go).
		dw, h = max(20, m.width-1), h+2
	}
	m.tasks.vp.Width, m.tasks.vp.Height = dw, h
	m.hw.vp.Width, m.hw.vp.Height = dw, h
	m.logs.vp.Width, m.logs.vp.Height = dw, h
	if m.logs.intro {
		m.logs.prodBody = logsIntro("Chọn một truy vấn bên trái rồi Enter (hoặc click lần hai).", [][2]string{
			{"/", "tìm tự do (bộ lọc prod của project được tự thêm vào)"},
			{"t", "đổi khoảng thời gian"},
			{"i", "kiến thức log production của project: namespace, trường log, request_id, cách điều tra"},
			{"o", "mở truy vấn vừa chạy trên Splunk Web"},
			{"c", "copy SPL"},
		}, dw)
	}
	if m.logs.env == envProd {
		if m.logs.intro {
			m.logs.vp.SetContent(m.logs.prodBody)
			m.logs.vpContent = m.logs.prodBody
		}
	} else {
		m.refreshQC()
	}
	sw, sh := max(20, m.width-1), h+2
	statusChanged := sw != m.stVP.Width || sh != m.stVP.Height
	m.stVP.Width, m.stVP.Height = sw, sh
	m.refreshTaskDetail(true)
	m.refreshHWDetail()
	if statusChanged {
		// Status takes the whole width: the divider does not move it, and it
		// is slow to build (it reads the runs' logs).
		m.refreshStatus()
	}
}

// tabAt maps a header x position to a tab.
func (m Model) tabAt(x int) (tabID, bool) {
	pos := 0
	for i := range tabNames {
		w := lipgloss.Width(m.tabLabel(tabID(i))) + 1
		if x >= pos && x < pos+w {
			return tabID(i), true
		}
		pos += w
	}
	return 0, false
}

func (m Model) tabLabel(t tabID) string {
	label := fmt.Sprintf(" %d %s ", int(t)+1, tabNames[t])
	if t == tabLogs {
		if n := m.logs.suggestions; n > 0 {
			label = fmt.Sprintf(" 3 Logs · %d đề xuất ", n)
		}
	}
	if t == tabTasks {
		if n := m.tasks.needsYou; n > 0 {
			label = fmt.Sprintf(" 1 Tasks ● %d cần bạn ", n)
		}
	}
	return label
}

// tabView renders a tab label: the number muted, the name bright and
// underlined when active, a count after it (amber when tasks wait on you).
// It is as wide as tabLabel, which the mouse hit test measures.
func (m Model) tabView(t tabID) string {
	label := strings.TrimSpace(m.tabLabel(t))
	num, rest, _ := strings.Cut(label, " ")
	name, extra := rest, ""
	for _, sep := range []string{" ● ", " · "} {
		if i := strings.Index(rest, sep); i >= 0 {
			name, extra = rest[:i], rest[i:]
			break
		}
	}
	nameStyle := tabStyle
	if t == m.tab {
		nameStyle = activeTabStyle
	}
	out := " " + dimStyle.Render(num) + " " + nameStyle.Render(name)
	switch {
	case strings.HasPrefix(extra, " ● "):
		out += " " + warnStyle.Render(strings.TrimPrefix(extra, " "))
	case extra != "":
		out += dimStyle.Render(extra)
	}
	return out + " "
}

// ---- update ---------------------------------------------------------------

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case clipPastedMsg:
		return m.onClipPasted(msg)
	case infoMsg:
		m.msg = string(msg)
		m.reloadTasks()
		m.refreshHWDetail()
		return m, nil
	case qcResultMsg:
		return m.onQCResult(msg)
	case qcStartedMsg:
		m.logs.qc.view, m.logs.qc.fetching = msg.view, true
		m.refreshQC()
		return m, nil
	case workDraftMsg:
		return m.onWorkDraft(msg)
	case assessedMsg:
		return m.onAssessed(msg)
	case otherAnswerMsg:
		return m.onOtherAnswer(msg)
	case investigateMsg:
		if m.modal != nil {
			return m, nil
		}
		return m.newTaskForm(msg.ticket, msg.hint)
	case taskDeletedMsg:
		return m.onTaskDeleted(msg)
	case hwSubtasksMsg:
		return m.onHWSubtasks(msg)
	case hwEpicsMsg:
		return m.onHWEpics(msg)
	case hwPushedMsg:
		delete(m.tasks.hwPushing, msg.taskID)
		delete(m.tasks.workDrafts, msg.taskID) // the next report drafts again
		m.msg = msg.msg
		m.reloadTasks()
		return m, nil
	case undoRequestedMsg:
		return m.onUndoRequested(msg)
	case tickMsg:
		m.logs.suggestions = m.suggestionCount("")
		m.reloadConfig()
		m.reloadTasks()
		m.refreshBanner()
		if m.tab == tabStatus {
			m.refreshStatus()
		}
		mm, cmd := m.assessNext()
		return mm, tea.Batch(tick(), cmd)

	case execDoneMsg:
		if msg.takeover != "" {
			m.msg = "đã thoát phiên làm tay; daemon có thể tiếp tục task " + msg.takeover
		}
		if msg.err != nil {
			m.msg = "lệnh kết thúc với lỗi: " + msg.err.Error()
		}
		m.refreshBanner()
		m.refreshStatus()
		m.reloadTasks()
		if m.mouseOff { // the terminal was restored with the startup mouse mode
			return m, tea.DisableMouse
		}
		return m, nil

	case cmdOutputMsg:
		out := strings.TrimRight(msg.out, "\n")
		if msg.err != nil {
			out += "\n\n" + errStyle.Render("✗ "+msg.err.Error())
		}
		vp := viewport.New(max(40, m.width-8), max(5, m.height-8))
		vp.SetContent(out)
		m.output = &outputView{title: msg.title, vp: vp}
		m.refreshBanner()
		m.refreshStatus()
		return m, nil

	case draftStartedMsg:
		m.tasks.drafting = true
		m.msg = "Claude đang soạn task từ prompt…"
		return m, nil
	case draftMsg:
		return m.onDraft(msg)
	case hwTasksMsg:
		return m.onHWTasks(msg)
	case hwDetailMsg:
		return m.onHWDetail(msg)
	case splGenStartedMsg:
		m.logs.generating = true
		m.msg = "Claude đang viết SPL…"
		return m, nil
	case splGenMsg:
		return m.onSPLGen(msg)
	case logsResultMsg:
		return m.onLogsResult(msg)
	case qcAppsMsg:
		return m.onQCApps(msg)
	case qcSyncMsg:
		return m.onQCSync(msg)

	case tea.MouseMsg:
		return m.onMouse(msg)

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m Model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.sel = selection{} // a key press ends a mouse selection
	// Overlays first, top-most wins.
	if m.conf != nil {
		switch k.String() {
		case "y", "Y", "enter":
			c := m.conf
			m.conf = nil
			return m, c.onYes()
		case "n", "N", "esc", "q":
			m.conf = nil
		}
		return m, nil
	}
	if m.output != nil {
		switch k.String() {
		case "esc", "q", "enter":
			m.output = nil
			return m, nil
		}
		var cmd tea.Cmd
		m.output.vp, cmd = m.output.vp.Update(k)
		return m, cmd
	}
	if m.modal != nil && k.Paste && m.modal.picker == nil {
		// A paste from the terminal (Ctrl+Shift+V, right click) arrives whole.
		return m.insertPaste(string(k.Runes)), nil
	}
	if m.modal != nil && (k.String() == "alt+v" || k.String() == "ctrl+v") && m.modal.picker == nil {
		return m.pasteIntoForm()
	}
	if m.modal != nil {
		cmd, done := m.modal.update(k)
		if done {
			m.modal = nil
			m.reloadTasks()
		}
		return m, cmd
	}
	if m.palette {
		return m.onPaletteKey(k)
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if m.tab == tabTasks && m.tasks.searching {
		return m.searchKey(k) // typed text, not shortcuts
	}
	// Questions waiting in the Tasks tab: digits pick, ←/→ change question,
	// 0 writes another answer, Enter sends (before the digits switch tabs).
	if m.tab == tabTasks {
		if mm, cmd, ok := m.inlineKey(k); ok {
			return mm, cmd
		}
	}

	switch k.String() {
	case "f3":
		return m.toggleZoom(), nil
	case "y":
		return m.copyDetail(), nil
	case "f2":
		m.mouseOff = !m.mouseOff
		if m.mouseOff {
			m.msg = "chuột trả cho terminal: kéo để bôi đen, copy như bình thường · F2 bật lại click/cuộn"
			return m, tea.DisableMouse
		}
		m.msg = "đã bật lại chuột trong TUI (giữ Shift khi kéo để vẫn bôi đen được)"
		return m, tea.EnableMouseCellMotion
	case "<", ">", "=":
		if mm, ok := m.splitKey(k.String()); ok {
			return mm, nil
		}
	case "ctrl+c", "q":
		return m.quit()
	case "1", "2", "3", "4":
		return m.switchTab(tabID(k.String()[0] - '1'))
	case "tab":
		return m.switchTab((m.tab + 1) % tabID(len(tabNames)))
	case "shift+tab":
		return m.switchTab((m.tab + tabID(len(tabNames)) - 1) % tabID(len(tabNames)))
	case ":":
		m.palette = true
		m.cmdline.SetValue("")
		m.histPos = len(m.history)
		return m, m.cmdline.Focus()
	case "?":
		m.help = true
		return m, nil
	}
	switch m.tab {
	case tabTasks:
		return m.tasksKey(k)
	case tabHasaki:
		return m.hwKey(k)
	case tabLogs:
		return m.logsKey(k)
	case tabStatus:
		return m.statusKey(k)
	}
	return m, nil
}

// reloadConfig picks up changes of the config file (e.g. a repo added), so
// the forms offer it without restarting the TUI.
func (m *Model) reloadConfig() {
	if m.d.Config == nil || m.d.Config.Path == "" {
		return
	}
	if m.cfgWatch == nil {
		m.cfgWatch = config.NewWatcher(m.d.Config)
	}
	c, err := m.cfgWatch.Check()
	switch {
	case err != nil:
		m.msg = "config vừa sửa bị lỗi, vẫn dùng bản cũ: " + err.Error()
	case c != nil:
		m.d.Config = c
		m.msg = "đã nạp lại config"
	}
}

func (m Model) switchTab(t tabID) (tea.Model, tea.Cmd) {
	m.tab = t
	m.msg = ""
	switch t {
	case tabHasaki:
		if !m.hw.loaded && !m.hw.loading {
			return m.loadHW()
		}
	case tabStatus:
		m.refreshStatus()
	case tabLogs:
		if m.logs.env == envQC {
			return m.ensureQCApps()
		}
	}
	return m, nil
}

func (m Model) quit() (tea.Model, tea.Cmd) {
	if m.d.Daemon.Embedded && m.d.Daemon.InFlight != nil {
		if n := m.d.Daemon.InFlight(); n > 0 {
			m.conf = &confirm{
				text:  fmt.Sprintf("%d task đang chạy trong TUI này. Thoát sẽ dừng chúng (chuyển sang needs_attention, gửi message để chạy tiếp sau).\nVẫn thoát?", n),
				onYes: func() tea.Cmd { return tea.Quit },
			}
			return m, nil
		}
	}
	return m, tea.Quit
}

func (m Model) onMouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.output != nil {
		var cmd tea.Cmd
		m.output.vp, cmd = m.output.vp.Update(ev)
		return m, cmd
	}
	if m.modal != nil || m.conf != nil || m.palette {
		return m, nil
	}
	if mm, cmd, ok := m.onSelect(ev); ok {
		return mm, cmd
	} else {
		m = mm
	}
	if m.zoom && m.tab != tabStatus && ev.Y > 0 {
		// One pane: the wheel scrolls it, clicks do nothing (no list).
		if ev.Button == tea.MouseButtonWheelUp || ev.Button == tea.MouseButtonWheelDown {
			m.zoomScroll(ev.Button == tea.MouseButtonWheelUp)
		}
		return m, nil
	}
	if mm, ok := m.onDividerDrag(ev); ok {
		return mm, nil
	}
	wheel := ev.Button == tea.MouseButtonWheelUp || ev.Button == tea.MouseButtonWheelDown
	up := ev.Button == tea.MouseButtonWheelUp
	if ev.Action != tea.MouseActionPress && !wheel {
		return m, nil
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if ev.Y == 0 && ev.Button == tea.MouseButtonLeft {
		if t, ok := m.tabAt(ev.X); ok {
			return m.switchTab(t)
		}
		return m, nil
	}
	inList := ev.X < m.listWidth()+2
	row := ev.Y - listTop
	switch m.tab {
	case tabTasks:
		if wheel && !inList {
			scroll(&m.tasks.vp, up)
			return m, nil
		}
		if wheel {
			m.moveTask(map[bool]int{true: -1, false: 1}[up])
			return m, nil
		}
		if inList && ev.Button == tea.MouseButtonLeft && row >= 0 && row < m.listRows() {
			all := m.tasks.rows()
			if lines := m.tasks.listLayout(all, m.listRows()); row < len(lines) && all[lines[row].row].task >= 0 {
				m.tasks.cursor = all[lines[row].row].task
				m.refreshTaskDetail(false)
			}
		}
	case tabHasaki:
		if wheel && !inList {
			scroll(&m.hw.vp, up)
			return m, nil
		}
		if wheel {
			return m.moveHW(map[bool]int{true: -1, false: 1}[up])
		}
		if inList && ev.Button == tea.MouseButtonLeft && row >= 0 && row < m.listRows() {
			if i := m.hw.offset(m.listRows()) + row/taskEntryLines; i < len(m.hw.list) {
				if i == m.hw.cursor {
					return m.dispatchHW() // click the selected row again to dispatch
				}
				m.hw.cursor = i
				return m.selectHW()
			}
		}
	case tabLogs:
		if wheel && !inList {
			scroll(&m.logs.vp, up)
			return m, nil
		}
		if !inList || ev.Button != tea.MouseButtonLeft {
			return m, nil
		}
		if ev.Y == 2 {
			return m.switchEnv()
		}
		// Same layout for both environments (logs_common.go).
		if m.logs.env == envQC {
			if i := m.logsRowAt(m.qcRows(), m.logs.qc.row, ev.Y); i >= 0 {
				if i == m.logs.qc.row {
					return m.qcRunRow()
				}
				m.logs.qc.row, m.logs.qc.groupsFocus = i, false
				if m.qcCurrent() != nil {
					m.logs.qc.view = qcViewApp
				}
				m.refreshQC()
			}
		} else if i := m.logsRowAt(m.prodRows(), m.logs.cursor, ev.Y); i >= 0 {
			if i == m.logs.cursor {
				return m.runLogItem()
			}
			m.logs.cursor, m.logs.groupsFocus = i, false
		}
	case tabStatus:
		if wheel {
			scroll(&m.stVP, up)
		}
	}
	return m, nil
}

// scroll moves a viewport by the mouse wheel.
func scroll(vp *viewport.Model, up bool) {
	if up {
		vp.ScrollUp(3)
	} else {
		vp.ScrollDown(3)
	}
}

// ---- command line -----------------------------------------------------------

var interactiveCommands = map[string]bool{
	"auth login": true, "auth set-token": true, "auth set-cookie": true, "auth bitbucket": true, "auth splunk": true,
	"auth argocd": true, "auth vault": true, "auth kafka": true, "auth claude": true,
}

func (m Model) onPaletteKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.palette = false
		m.cmdline.Blur()
		return m, nil
	case "up":
		if m.histPos > 0 {
			m.histPos--
			m.cmdline.SetValue(m.history[m.histPos])
			m.cmdline.CursorEnd()
		}
		return m, nil
	case "down":
		if m.histPos < len(m.history)-1 {
			m.histPos++
			m.cmdline.SetValue(m.history[m.histPos])
		} else {
			m.histPos = len(m.history)
			m.cmdline.SetValue("")
		}
		return m, nil
	case "enter":
		line := strings.TrimSpace(m.cmdline.Value())
		m.palette = false
		m.cmdline.Blur()
		if line == "" {
			return m, nil
		}
		m.history = append(m.history, line)
		return m.runCommand(line)
	}
	var cmd tea.Cmd
	m.cmdline, cmd = m.cmdline.Update(k)
	return m, cmd
}

func (m Model) runCommand(line string) (tea.Model, tea.Cmd) {
	args := splitArgs(strings.TrimPrefix(line, "taskpilot "))
	if len(args) == 0 {
		return m, nil
	}
	switch args[0] {
	case "q", "quit", "exit":
		return m.quit()
	case "tasks":
		return m.switchTab(tabTasks)
	case "hw", "hasaki":
		if len(args) == 1 {
			return m.switchTab(tabHasaki)
		}
	case "status":
		return m.switchTab(tabStatus)
	case "logs":
		return m.switchTab(tabLogs)
	case "help", "?":
		m.help = true
		return m, nil
	case "tui", "daemon":
		m.msg = "`" + args[0] + "` không chạy được bên trong TUI (TUI đã có daemon riêng)"
		return m, nil
	}
	if m.d.Exe == "" {
		m.msg = "không tìm thấy binary taskpilot để chạy lệnh"
		return m, nil
	}
	cmd := m.execTaskpilot(args...)
	if len(args) >= 2 && interactiveCommands[args[0]+" "+args[1]] {
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err: err} })
	}
	m.msg = "đang chạy: taskpilot " + strings.Join(args, " ")
	return m, func() tea.Msg {
		out, err := cmd.CombinedOutput()
		return cmdOutputMsg{title: "taskpilot " + strings.Join(args, " "), out: string(out), err: err}
	}
}

// splitArgs splits a command line on spaces, honouring single and double quotes.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inArg := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inArg = r, true
		case r == ' ' || r == '\t':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteRune(r)
			inArg = true
		}
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out
}

// ---- view -------------------------------------------------------------------

// View implements tea.Model.
func (m Model) View() string {
	screen := m.dragGuide(m.highlight(m.screen()))

	var overlay string
	switch {
	case m.conf != nil:
		overlay = m.conf.view(m.width)
	case m.modal != nil:
		overlay = m.modal.view(m.width)
	case m.output != nil:
		overlay = modalStyle.Render(titleStyle.Render(m.output.title) + "\n\n" + m.output.vp.View() +
			"\n" + dimStyle.Render("↑/↓/PgUp/PgDn/chuột: cuộn · Esc/Enter: đóng"))
	case m.help:
		overlay = m.helpView()
	}
	if overlay != "" {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, overlay,
			lipgloss.WithWhitespaceChars(" "))
	}
	return screen
}

func (m Model) headerView() string {
	var tabs []string
	for i := range tabNames {
		tabs = append(tabs, m.tabView(tabID(i)))
	}
	left := strings.Join(tabs, " ")
	daemon := ""
	if d := m.d.Daemon; d.Embedded && d.InFlight != nil {
		daemon = fmt.Sprintf("daemon: %d đang chạy", d.InFlight())
	} else if d.External && d.InHerdr {
		daemon = "daemon: tab herdr"
	} else if d.External {
		daemon = "daemon: tiến trình khác"
	} else {
		daemon = "daemon: không chạy"
	}
	// Fit the right side into the space left by the tabs, shortening the banner first.
	avail := m.width - lipgloss.Width(left) - 2
	banner := m.bannerText
	if room := avail - len([]rune(daemon)) - 3; len([]rune(banner)) > room {
		banner = truncate(banner, max(0, room))
	}
	right := ""
	if banner != "" {
		style := dimStyle
		if m.bannerWarn {
			style = warnStyle
		}
		right = style.Render(banner) + dimStyle.Render(" · ")
	}
	if strings.HasSuffix(daemon, "không chạy") {
		right += warnStyle.Render(daemon)
	} else {
		right += dimStyle.Render(daemon)
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) footerView() string {
	if m.palette {
		return m.cmdline.View() + dimStyle.Render("   (lệnh taskpilot, vd: hw show 14352972 · doctor · auth status · Esc huỷ)")
	}
	keys := ""
	switch m.tab {
	case tabTasks:
		keys = "↑↓ chọn · n task mới · d xoá · c xong · a duyệt · x từ chối · m nhắn · u thu hồi · t trò chuyện · Enter cuộn chi tiết · / tìm · p PR · h Hasaki Work · A duyệt+ghi chú · H subtask · o làm tay · v hoạt động · w/W xem code · e model · N task từ prompt · f lọc"
		if p := m.tasks.pending; p != nil && len(p.Questions) > 0 {
			keys = "câu hỏi: " + inlineKeys(p) + " · ↑↓ task khác"
		}
		if m.tasks.focus {
			keys = "chi tiết: j/k ↑↓ cuộn · PgUp/PgDn nửa trang · g/G đầu/cuối · t Tổng quan/Trò chuyện · Esc/← về danh sách"
		}
		if m.tasks.drafting {
			keys = "Claude đang soạn task từ prompt… · " + keys
		}
	case tabHasaki:
		keys = "↑↓ chọn · Enter giao cho agent · a gộp vào task đang làm · n tạo subtask · s lọc trạng thái · r tải lại"
	case tabLogs:
		common := "↑↓ chọn · Enter chạy · g nhóm lỗi · n điều tra · / tìm · t/T thời gian · r chạy lại · c copy"
		if m.logs.env == envQC {
			keys = "e Production · ←/→ project · " + common + " · o Argo CD · S sync"
		} else {
			keys = "e QC · ←/→ project · " + common + " · a hỏi bằng lời · o Splunk Web · i kiến thức"
		}
		if m.logs.suggFocus {
			keys = "đề xuất: " + suggestKeys
		} else if m.logs.groupsFocus || m.logs.qc.groupsFocus {
			keys = "nhóm lỗi: ↑↓ chọn · Enter xem log · n điều tra · f sửa ngay · s chỉ service này · esc về danh sách"
		}
		if m.logs.generating {
			keys = "Claude đang viết SPL… · " + keys
		}
		if m.logs.running || m.logs.qc.fetching {
			keys = "đang đọc log… · " + keys
		}
	case tabStatus:
		keys = "l Hasaki · s/S Splunk (S: tạo token) · a Argo CD · v Vault QC · K Kafka UI · t dán token · b Bitbucket · d doctor · r làm mới"
	}
	// The tab's keys come first and give way; the way to the rest stays at the
	// right edge (mouse, F3, y, </> and : are in the help).
	const tail = "? trợ giúp · q thoát"
	if m.zoom {
		keys = "[khung chi tiết toàn màn hình: Shift + kéo để bôi đen · F3 trở lại] · " + keys
	}
	if m.mouseOff {
		keys = "[chuột: bôi đen, F2 tắt] · " + keys
	}
	if m.msg != "" {
		return truncate(accentStyle.Render(m.msg)+"   "+dimStyle.Render(keys+" · "+tail), m.width+40)
	}
	room := m.width - lipgloss.Width(tail) - 2
	keys = truncate(keys, max(0, room))
	gap := max(2, m.width-lipgloss.Width(keys)-lipgloss.Width(tail))
	return dimStyle.Render(keys + strings.Repeat(" ", gap) + tail)
}

func (m Model) helpView() string {
	h := `taskpilot · phím tắt (bấm phím hoặc click)

Chung      1 2 3 4 / Tab: đổi tab (hoặc click tên tab) · : gõ lệnh taskpilot bất kỳ
           Kéo chuột (không cần Shift) để bôi đen: vùng chọn chỉ nằm trong khung bạn bắt đầu kéo (danh sách hoặc chi tiết),
             thả chuột là copy vào clipboard · Shift + kéo vẫn là bôi đen của terminal (lấy cả hàng màn hình)
           F3 phóng to khung chi tiết (ẩn danh sách và viền: bôi đen copy không dính panel khác) · y copy cả khung chi tiết
           < > thu hẹp / mở rộng panel bên trái · = về mặc định · hoặc kéo đường chia giữa hai panel bằng chuột (được nhớ)
           F2 bật/tắt chuột trong TUI: tắt để kéo bôi đen và copy (hoặc giữ Shift khi kéo)
           ? trợ giúp · q thoát

Tasks      ↑↓ / click: chọn task · cuộn chuột bên phải: đọc plan/diff dài
           Enter / → : chuyển phím sang khung chi tiết (j/k cuộn, g/G đầu/cuối, Esc/← quay lại) · Space/PgDn cuộn nửa trang
           t Tổng quan ⇄ Trò chuyện: toàn bộ hội thoại với agent theo từng lượt chạy (agent viết gì, làm gì,
             hỏi/xin duyệt gì, bạn trả lời/duyệt/nhắn gì) · Level (● Nhẹ/Trung bình/Khó/Rất khó) và loại task do agent đánh giá
           a duyệt yêu cầu đang chờ · A duyệt kèm ghi chú · x từ chối kèm nhận xét
           m nhắn agent (task đang dừng) · p mở PR / trang tạo PR · v mở/thu gọn Hoạt động
           u thu hồi tin nhắn / quyết định cuối bạn gửi, bất cứ lúc nào: dừng lượt agent đang chạy, tua session về trước nó,
             rồi mở lại form với nội dung cũ để sửa (file agent đã sửa trong lượt đó vẫn còn)
           h Hasaki Work: task tạo trong taskpilot → tạo task trên Work (EPIC, sprint, giao cho bạn) và ghi kết quả + đánh hoàn thành/chờ duyệt;
             task lấy từ Work → tạo SUBTASK dưới nó, giao cho bạn, ghi kết quả + giờ thực tế, đánh hoàn thành subtask
             (task cha giữ nguyên) · H tạo nhiều subtask trống cho task lấy từ Work
           (tạo task:) chọn dự án + nhập tên, EPIC (DEV / Improve / Issue Production)
             và sprint hiện tại được điền tự động, task giao cho bạn
           o làm tay trong session của agent · n tạo task mới · f chỉ hiện task cần bạn
           / tìm task (theo id, tên, project, nhánh, ticket; gõ không dấu được) · Esc xoá bộ lọc
           w mở workspace của task trong Cursor/VS Code (xem thay đổi của agent, không cần checkout nhánh)
           W chuyển repo chính của bạn sang commit mới nhất của task (detached; repo phải sạch); W lần nữa: cập nhật / quay về
           e chọn model/effort cho task (ghim, hoặc để agent tự chọn sau khi đánh giá task)
           c đánh dấu task xong bằng tay (kèm ghi chú) · d xoá task (hỏi lại trước; xoá cả workspace, giữ nhánh git và PR)
           danh sách chia hai nhóm: Đang làm ở trên, Đã xong ở dưới
           N tạo task từ prompt: mô tả bằng lời, Claude điền project/repo/tiêu đề/mô tả để bạn sửa rồi gửi

Hasaki     ↑↓ / click: chọn task · Enter (hoặc click lần 2): giao cho agent
           a gộp vào task đang làm (vd bug của tính năng task đó): agent nhận ticket như tin nhắn, cùng session + branch
           n tạo subtask dưới task đang chọn (mỗi dòng một subtask, cùng dự án + sprint, giao cho bạn)
           s đổi bộ lọc (đang mở / chờ duyệt / đã xong) · r tải lại

Logs       e đổi môi trường: QC (Argo CD, cả 3 project) ↔ Production (Splunk, bom + payment) — cùng bố cục, cùng phím:
           ←/→ project · ↑↓ chọn · Enter chạy · t/T thời gian (15 phút … hôm nay, hôm qua, 7 ngày) · r chạy lại · c copy
           g nhóm lỗi theo service: ↑↓ chọn nhóm · Enter xem log · n điều tra · f sửa ngay · s chỉ service đó · esc
           n điều tra sự cố: từ nhóm lỗi, từ log đang xem, hoặc mục Điều tra sự cố (mô tả triệu chứng) — agent tìm nguyên nhân gốc,
             báo cáo ở cổng plan, chỉ sửa khi bạn duyệt
           / tìm theo điều kiện: service, mức, chuỗi, request_id
  QC       log đọc từ Argo CD (không có trên Splunk) · danh sách app: Enter xem log pod · S sync · o mở Argo CD
  Prod     truy vấn có sẵn (hỏi tham số) · a hỏi bằng lời (Claude viết SPL) · o Splunk Web · i kiến thức log

Status     l đăng nhập Hasaki Work · s đăng nhập Splunk · S đăng nhập + tạo token Splunk · t dán JWT · b lưu token Bitbucket · d doctor
           : splunk search "<SPL>" -earliest -1h   tự tìm log

Cổng       plan → pr (agent mở PR vào release/qc) → qc (QC đạt: PR vào main)

Nhấn phím bất kỳ để đóng.`
	return modalStyle.Render(h)
}

// ---- styles -----------------------------------------------------------------

var (
	titleStyle     = fg(cBright)
	dimStyle       = fg(cMuted)
	accentStyle    = fg(cBright) // highlighted text; amber is warnStyle
	warnStyle      = fg(cAccent)
	errStyle       = lipgloss.NewStyle().Foreground(clr(cErr))
	selStyle       = lipgloss.NewStyle().Foreground(clr(cBright)).Background(clr(cSurface))
	boxStyle       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clr(cRule)).Padding(0, 1)
	modalStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(clr(cSub)).Padding(1, 2)
	tabStyle       = fg(cSub)
	activeTabStyle = fg(cBright).Underline(true)
	stateColors    = map[store.State]string{
		store.StateQueued:         cMuted,
		store.StatePreparing:      cMuted,
		store.StateRunning:        cBright,
		store.StateAwaitApproval:  cAccent,
		store.StateNeedsAttention: cAccent,
		store.StateDone:           cOk,
		store.StateFailed:         cErr,
	}
)

func stateBadge(s store.State) string {
	return lipgloss.NewStyle().Foreground(clr(stateColors[s])).Render(string(s))
}
