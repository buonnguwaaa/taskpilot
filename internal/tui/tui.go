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
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/desktop"
	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// HW is the part of the Hasaki Work client the TUI uses.
type HW interface {
	MyTasks(ctx context.Context, statusFilter string) ([]hasakiwork.Task, error)
	Task(ctx context.Context, id int64) (*hasakiwork.Task, error)
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
	// AuthStatus returns the lines of the status page's login section.
	AuthStatus func() []string
	OpenURL    func(string) error
	Copy       func(string) error
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
	d Deps

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

// ---- layout ---------------------------------------------------------------

func (m Model) listWidth() int   { return max(34, m.width*2/5) }
func (m Model) innerHeight() int { return max(3, m.height-4) } // header + footer + box borders

// listRows is how many list entries fit under a list box's title line.
func (m Model) listRows() int { return m.innerHeight() - 1 }

// listTop is the screen row of the first list entry: header, box border, list title.
const listTop = 3

func (m *Model) resize() {
	dw := max(20, m.width-m.listWidth()-6)
	h := m.innerHeight()
	m.tasks.vp.Width, m.tasks.vp.Height = dw, h
	m.hw.vp.Width, m.hw.vp.Height = dw, h
	m.logs.vp.Width, m.logs.vp.Height = dw, h
	if m.logs.intro {
		m.logs.prodBody = dimStyle.Render(wrap("Chọn một truy vấn bên trái rồi Enter (hoặc click lần hai).\n\n"+
			"/  tìm tự do (bộ lọc prod của project được tự thêm vào)\n"+
			"t  đổi khoảng thời gian\n"+
			"i  kiến thức log production của project: namespace, trường log, request_id, cách điều tra\n"+
			"o  mở truy vấn vừa chạy trên Splunk Web · c copy SPL", dw))
	}
	if m.logs.env == envProd {
		if m.logs.intro {
			m.logs.vp.SetContent(m.logs.prodBody)
			m.logs.vpContent = m.logs.prodBody
		}
	} else {
		m.refreshQC()
	}
	m.stVP.Width, m.stVP.Height = max(20, m.width-4), h
	m.refreshTaskDetail(true)
	m.refreshHWDetail(true)
	m.refreshStatus()
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
	if t == tabTasks {
		if n := m.tasks.needsYou; n > 0 {
			label = fmt.Sprintf(" 1 Tasks (%d cần bạn) ", n)
		}
	}
	return label
}

// ---- update ---------------------------------------------------------------

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		return m, nil

	case infoMsg:
		m.msg = string(msg)
		m.reloadTasks()
		return m, nil
	case tickMsg:
		m.reloadTasks()
		m.refreshBanner()
		if m.tab == tabStatus {
			m.refreshStatus()
		}
		return m, tick()

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

	case hwTasksMsg:
		return m.onHWTasks(msg)
	case hwDetailMsg:
		return m.onHWDetail(msg)
	case logsResultMsg:
		return m.onLogsResult(msg)
	case qcAppsMsg:
		return m.onQCApps(msg)
	case qcLogsMsg:
		return m.onQCLogs(msg)
	case qcSyncMsg:
		return m.onQCSync(msg)
	case qcFilterMsg:
		m.logs.qc.filter = string(msg)
		m.refreshQC()
		return m, nil

	case tea.MouseMsg:
		return m.onMouse(msg)

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m Model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
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

	switch k.String() {
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
			if up {
				m.tasks.vp.ScrollUp(3)
			} else {
				m.tasks.vp.ScrollDown(3)
			}
			return m, nil
		}
		if wheel {
			m.moveTask(map[bool]int{true: -1, false: 1}[up])
			return m, nil
		}
		if inList && ev.Button == tea.MouseButtonLeft && row >= 0 && row < m.listRows() {
			if i := m.tasks.offset(m.listRows()) + row; i < len(m.tasks.list) {
				m.tasks.cursor = i
				m.refreshTaskDetail(false)
			}
		}
	case tabHasaki:
		if wheel && !inList {
			if up {
				m.hw.vp.ScrollUp(3)
			} else {
				m.hw.vp.ScrollDown(3)
			}
			return m, nil
		}
		if wheel {
			return m.moveHW(map[bool]int{true: -1, false: 1}[up])
		}
		if inList && ev.Button == tea.MouseButtonLeft && row >= 0 && row < m.listRows() {
			if i := m.hw.offset(m.listRows()) + row; i < len(m.hw.list) {
				if i == m.hw.cursor {
					return m.dispatchHW() // click the selected row again to dispatch
				}
				m.hw.cursor = i
				return m.selectHW()
			}
		}
	case tabLogs:
		if wheel && !inList {
			if up {
				m.logs.vp.ScrollUp(3)
			} else {
				m.logs.vp.ScrollDown(3)
			}
			return m, nil
		}
		// List rows start at y=4: header, box border, environment line, project line.
		r := ev.Y - 4
		if !inList || ev.Button != tea.MouseButtonLeft {
			return m, nil
		}
		if ev.Y == 2 {
			return m.switchEnv()
		}
		if m.logs.env == envQC {
			if r >= 0 && r < len(m.qcApps()) {
				if r == m.logs.qc.cursor {
					return m.loadQCLogs()
				}
				m.logs.qc.cursor = r
				m.refreshQC()
			}
		} else if r >= 0 && r < len(m.logItems()) {
			if r == m.logs.cursor {
				return m.runLogItem()
			}
			m.logs.cursor = r
		}
	case tabStatus:
		if wheel {
			if up {
				m.stVP.ScrollUp(3)
			} else {
				m.stVP.ScrollDown(3)
			}
		}
	}
	return m, nil
}

// ---- command line -----------------------------------------------------------

var interactiveCommands = map[string]bool{
	"auth login": true, "auth set-token": true, "auth set-cookie": true, "auth bitbucket": true, "auth splunk": true,
	"auth argocd": true,
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
	header := m.headerView()
	var body string
	switch m.tab {
	case tabTasks:
		body = m.tasksView()
	case tabHasaki:
		body = m.hwView()
	case tabLogs:
		body = m.logsView()
	case tabStatus:
		body = boxStyle.Width(m.width - 2).Height(m.innerHeight()).Render(m.stVP.View())
	}
	footer := m.footerView()
	screen := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)

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
		label := m.tabLabel(tabID(i))
		if tabID(i) == m.tab {
			tabs = append(tabs, activeTabStyle.Render(label))
		} else {
			tabs = append(tabs, tabStyle.Render(label))
		}
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
		keys = "↑↓ chọn · a duyệt · A duyệt+ghi chú · x từ chối · m nhắn · p PR · o làm tay · n task mới · f lọc"
	case tabHasaki:
		keys = "↑↓ chọn · Enter giao cho agent · s lọc trạng thái · r tải lại"
	case tabLogs:
		if m.logs.env == envQC {
			keys = "e Production · ←/→ project · ↑↓ app · Enter log · / lọc · t thời gian · o Argo CD · S sync · r tải lại"
		} else {
			keys = "e QC · ←/→ project · ↑↓ chọn · Enter chạy · / tìm tự do · t thời gian · o Splunk Web · c copy · i kiến thức"
			if m.logs.running {
				keys = "đang tìm trên Splunk… · " + keys
			}
		}
	case tabStatus:
		keys = "l Hasaki · s/S Splunk (S: tạo token) · a Argo CD · t dán token · b Bitbucket · d doctor · r làm mới"
	}
	keys += " · 1-4 tab · : lệnh · ? trợ giúp · q thoát"
	if m.msg != "" {
		return truncate(accentStyle.Render(m.msg)+"   "+dimStyle.Render(keys), m.width+40)
	}
	return dimStyle.Render(truncate(keys, m.width))
}

func (m Model) helpView() string {
	h := `taskpilot — làm mọi thứ bằng phím hoặc chuột

Chung      1 2 3 4 / Tab: đổi tab (hoặc click tên tab) · : gõ lệnh taskpilot bất kỳ
           ? trợ giúp · q thoát

Tasks      ↑↓ / click: chọn task · cuộn chuột bên phải: đọc plan/diff dài
           a duyệt yêu cầu đang chờ · A duyệt kèm ghi chú · x từ chối kèm nhận xét
           m nhắn agent (task đang dừng) · p mở PR / trang tạo PR
           o làm tay trong session của agent · n tạo task mới · f chỉ hiện task cần bạn

Hasaki     ↑↓ / click: chọn task · Enter (hoặc click lần 2): giao cho agent
           s đổi bộ lọc (đang mở / chờ duyệt / đã xong) · r tải lại

Logs       e đổi môi trường: QC (Argo CD, cả 3 project) ↔ Production (Splunk, bom + payment)
  QC       ←/→ project · ↑↓ app · Enter xem log pod · / lọc · t khoảng thời gian
           o mở app trên Argo CD · S sync (hỏi xác nhận) · r tải lại trạng thái
  Prod     ←/→ project · ↑↓ truy vấn · Enter chạy (hỏi tham số) · / tìm tự do · t thời gian
           o mở trên Splunk Web · c copy SPL · i kiến thức log production

Status     l đăng nhập Hasaki Work · s đăng nhập Splunk · S đăng nhập + tạo token Splunk · t dán JWT · b lưu token Bitbucket · d doctor
           : splunk search "<SPL>" -earliest -1h   tự tìm log

Cổng       plan → pr (agent mở PR vào release/qc) → qc (QC đạt: PR vào main)

Nhấn phím bất kỳ để đóng.`
	return modalStyle.Render(h)
}

// ---- styles -----------------------------------------------------------------

var (
	titleStyle     = lipgloss.NewStyle().Bold(true)
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	accentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	warnStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("208"))
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	selStyle       = lipgloss.NewStyle().Reverse(true)
	boxStyle       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	modalStyle     = lipgloss.NewStyle().Border(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("12")).Padding(1, 2)
	tabStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	activeTabStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
	stateColors    = map[store.State]string{
		store.StateQueued:         "12",
		store.StatePreparing:      "12",
		store.StateRunning:        "14",
		store.StateAwaitApproval:  "11",
		store.StateNeedsAttention: "208",
		store.StateDone:           "10",
		store.StateFailed:         "9",
	}
)

func stateBadge(s store.State) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(stateColors[s])).Render(string(s))
}
