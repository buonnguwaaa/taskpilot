package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/plugin"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splgen"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// logsMaxEvents caps one search from the TUI.
const logsMaxEvents = 200

// groupsLimit caps the error groups of one grouping search.
const groupsLimit = 60

type (
	logsResultMsg struct {
		spl, earliest, latest string
		groups                bool // the search is the error grouping (rows are groups)
		events                []splunk.Event
		took                  time.Duration
		err                   error
	}
	// splGenStartedMsg marks that Claude started writing an SPL.
	splGenStartedMsg struct{}
	// splGenMsg brings an SPL generated from a question.
	splGenMsg struct {
		question string
		res      *splgen.Result
		err      error
	}
)

type logsEnv int

const (
	envQC logsEnv = iota
	envProd
)

type logsTab struct {
	env      logsEnv
	prodBody string // right pane content of each environment, kept when switching
	qcBody   string
	qc       qcState

	proj    int
	cursor  int
	preset  int // 0: each saved search's own window; i>0: splunk.Presets[i-1]
	running bool
	vp      viewport.Model

	lastSPL, lastEarliest, lastLatest string
	lastGroups                        bool           // the last search was the error grouping
	lastEvents                        []splunk.Event // rows of the last search, for an investigation ticket
	intro                             bool           // the help text is shown until the first search
	vpContent                         string

	// Error groups of the last grouping search; groupsFocus moves the cursor
	// (and the keys) to the right pane.
	groups      []splunk.ErrorGroup
	groupCursor int
	groupsFocus bool
	generating  bool // claude is writing an SPL from a question
}

// Special entries of the saved-search list.
const (
	itemGroups      = "⚠ Nhóm lỗi theo service"
	itemInvestigate = "🔎 Điều tra sự cố…"
	itemBuilder     = "✎ Tìm theo điều kiện…"
	itemAsk         = "✨ Hỏi bằng lời (Claude viết SPL)…"
)

func (m Model) logProjects() []string { return m.d.Config.LogProjects() }

func (m Model) logProject() (string, *config.ProdLogs) {
	ps := m.logProjects()
	if len(ps) == 0 {
		return "", nil
	}
	name := ps[m.logs.proj%len(ps)]
	return name, m.d.Config.Projects[name].LogsProduction
}

// logItems are the error grouping, the saved searches, the builder and the
// question to Claude.
func (m Model) logItems() []config.LogQuery {
	_, pl := m.logProject()
	if pl == nil {
		return nil
	}
	items := []config.LogQuery{{Name: itemGroups, Note: "Gom lỗi giống nhau (số, mã, UUID được che) của mọi service; chọn một nhóm để xem log hoặc tạo task sửa."}}
	items = append(items, config.LogQuery{Name: itemInvestigate, Note: "Khách / vận hành báo lỗi? Mô tả triệu chứng, mã đơn/giao dịch, khoảng thời gian: tạo task để agent điều tra nguyên nhân gốc trên Splunk, code và DB QC, báo cáo trước khi sửa."})
	items = append(items, pl.Queries...)
	items = append(items,
		config.LogQuery{Name: itemBuilder, Note: "Chọn service, mức, chuỗi cần tìm, request_id: taskpilot tự viết SPL."},
		config.LogQuery{Name: itemAsk, Note: "Mô tả bằng lời (vd \"refund TCB lỗi sáng nay\"); Claude đọc tài liệu log của dự án và viết SPL để bạn sửa trước khi chạy."})
	return items
}

// window is the time range for a search: the chosen preset, or the saved
// search's own earliest (then the config default).
func (m Model) window(q config.LogQuery) (earliest, latest string) {
	if m.logs.preset > 0 {
		p := splunk.Presets[m.logs.preset-1]
		return p.Earliest, p.Latest
	}
	if q.Earliest != "" {
		return q.Earliest, ""
	}
	return m.d.Config.Splunk.DefaultEarliest, ""
}

func (m Model) presetLabel() string {
	if m.logs.preset == 0 {
		return "theo truy vấn"
	}
	return splunk.Presets[m.logs.preset-1].Label
}

// presetOptions are the windows of the forms, with the current one first chosen.
func presetOptions(*form) []option {
	var o []option
	for _, p := range splunk.Presets {
		o = append(o, option{Label: p.Label, Value: p.Earliest + "|" + p.Latest})
	}
	return o
}

func splitWindow(v string) (string, string) {
	e, l, _ := strings.Cut(v, "|")
	return e, l
}

// envLine is the first line of the Logs list box: which environment is shown.
func (m Model) envLine() string {
	qc := " QC · Argo CD "
	prod := " Production · Splunk "
	if m.logs.env == envQC {
		return activeTabStyle.Render(qc) + " " + tabStyle.Render(prod) + dimStyle.Render("  e đổi")
	}
	return tabStyle.Render(qc) + " " + activeTabStyle.Render(prod) + dimStyle.Render("  e đổi")
}

func (m Model) logsView() string {
	if m.logs.env == envQC {
		return m.qcView()
	}
	return m.prodView()
}

func (m Model) logsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "e" && !m.logs.groupsFocus {
		return m.switchEnv()
	}
	if m.logs.env == envQC {
		return m.qcKey(k)
	}
	return m.prodKey(k)
}

func (m Model) switchEnv() (tea.Model, tea.Cmd) {
	if m.logs.env == envQC {
		m.logs.qcBody = m.logs.vpContent
		m.logs.env = envProd
		m.logs.vp.SetContent(m.logs.prodBody)
		m.logs.vpContent = m.logs.prodBody
		return m, nil
	}
	m.logs.prodBody = m.logs.vpContent
	m.logs.env = envQC
	m.logs.groupsFocus = false
	m.logs.vp.SetContent(m.logs.qcBody)
	m.logs.vpContent = m.logs.qcBody
	return m.ensureQCApps()
}

func (m Model) prodView() string {
	h := m.innerHeight()
	lw := m.listWidth()
	name, pl := m.logProject()
	var b strings.Builder
	b.WriteString(m.envLine() + "\n")
	if pl == nil {
		b.WriteString(dimStyle.Render("Chưa có project nào có logs_production trong config."))
		return boxStyle.Width(m.width - 2).Height(h).Render(b.String())
	}
	var tabs []string
	for i, p := range m.logProjects() {
		if i == m.logs.proj%len(m.logProjects()) {
			tabs = append(tabs, activeTabStyle.Render(" "+p+" "))
		} else {
			tabs = append(tabs, tabStyle.Render(" "+p+" "))
		}
	}
	b.WriteString(strings.Join(tabs, " ") + dimStyle.Render("  ←/→") + "\n")
	b.WriteString(dimStyle.Render("thời gian: ") + accentStyle.Render(m.presetLabel()) + dimStyle.Render("  (t đổi)") + "\n")
	items := m.logItems()
	rows := m.listRows() - 6
	start := 0
	if m.logs.cursor >= rows {
		start = m.logs.cursor - rows + 1
	}
	for i := start; i < len(items) && i-start < rows; i++ {
		label := truncate(items[i].Name, lw-4)
		switch {
		case i == m.logs.cursor && !m.logs.groupsFocus:
			b.WriteString(selStyle.Render(" "+label+" ") + "\n")
		case i == m.logs.cursor:
			b.WriteString(accentStyle.Render("›"+label) + "\n")
		default:
			b.WriteString(" " + label + "\n")
		}
	}
	if m.logs.cursor < len(items) && !m.logs.groupsFocus {
		q := items[m.logs.cursor]
		b.WriteString("\n")
		if q.Note != "" {
			b.WriteString(dimStyle.Render(wrap(q.Note, lw-2)) + "\n")
		}
		if q.SPL != "" {
			b.WriteString(dimStyle.Render(wrap("SPL: "+q.SPL, lw-2)) + "\n")
		}
	}
	if pl.Filter == "" {
		b.WriteString("\n" + warnStyle.Render(wrap(fmt.Sprintf("Chưa có filter prod cho %s: chạy truy vấn tìm namespace rồi đặt logs_production.filter trong config.", name), lw-2)))
	}
	left := boxStyle.Width(lw).Height(h).Render(b.String())
	right := boxStyle.Width(m.logs.vp.Width + 2).Height(h).Render(m.logs.vp.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) prodKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.logs.groupsFocus {
		if mm, cmd, ok := m.groupsKey(k); ok {
			return mm, cmd
		}
	}
	items := m.logItems()
	switch k.String() {
	case "left", "h", "[":
		if n := len(m.logProjects()); n > 0 {
			m.logs.proj = (m.logs.proj + n - 1) % n
			m.logs.cursor = 0
		}
	case "right", "l", "]":
		if n := len(m.logProjects()); n > 0 {
			m.logs.proj = (m.logs.proj + 1) % n
			m.logs.cursor = 0
		}
	case "j", "down":
		m.logs.cursor = min(m.logs.cursor+1, max(0, len(items)-1))
	case "k", "up":
		m.logs.cursor = max(0, m.logs.cursor-1)
	case "t":
		m.logs.preset = (m.logs.preset + 1) % (len(splunk.Presets) + 1)
		m.msg = "thời gian: " + m.presetLabel() + " (áp dụng cho lần tìm tiếp theo; r để chạy lại)"
	case "T":
		m.logs.preset = (m.logs.preset + len(splunk.Presets)) % (len(splunk.Presets) + 1)
		m.msg = "thời gian: " + m.presetLabel()
	case "pgdown", "ctrl+d", " ":
		m.logs.vp.HalfPageDown()
	case "pgup", "ctrl+u":
		m.logs.vp.HalfPageUp()
	case "enter":
		return m.runLogItem()
	case "/":
		m.logs.cursor = len(items) - 2 // the builder
		return m.runLogItem()
	case "a":
		m.logs.cursor = len(items) - 1 // the question to Claude
		return m.runLogItem()
	case "g":
		if len(m.logs.groups) > 0 { // back to the last groups; r or Enter on the entry searches again
			m.logs.groupsFocus = true
			m.renderGroups()
			return m, nil
		}
		m.logs.cursor = 0
		return m.runLogItem()
	case "r":
		if m.logs.lastSPL != "" {
			e, l := m.logs.lastEarliest, m.logs.lastLatest
			if m.logs.preset > 0 { // a new preset applies to the rerun
				p := splunk.Presets[m.logs.preset-1]
				e, l = p.Earliest, p.Latest
			}
			return m.runLogSearch(m.logs.lastSPL, e, l, m.logs.lastGroups)
		}
	case "o":
		if m.logs.lastSPL == "" {
			m.msg = "chưa chạy truy vấn nào"
			break
		}
		if err := m.d.OpenURL(splunk.WebURLRange(m.d.Config.Splunk.URL, m.d.Config.Splunk.Locale, m.logs.lastSPL, m.logs.lastEarliest, m.logs.lastLatest)); err != nil {
			m.msg = "mở trình duyệt: " + err.Error()
		} else {
			m.msg = "đã mở truy vấn trên Splunk Web"
		}
	case "c":
		if m.logs.lastSPL == "" {
			m.msg = "chưa chạy truy vấn nào"
			break
		}
		if err := m.d.Copy(m.logs.lastSPL); err != nil {
			m.msg = "copy: " + err.Error()
		} else {
			m.msg = "đã copy SPL"
		}
	case "n":
		if m.logs.lastSPL != "" && !m.logs.lastGroups {
			return m.investigateFromResults()
		}
		return m.investigateForm()
	case "i":
		_, pl := m.logProject()
		if pl == nil || pl.Knowledge == "" {
			m.msg = "project này chưa có tài liệu log"
			break
		}
		text, err := plugin.Read("skills/prod-logs/" + pl.Knowledge)
		if err != nil {
			m.msg = "đọc tài liệu: " + err.Error()
			break
		}
		return m.Update(cmdOutputMsg{title: "Kiến thức log production — " + pl.Knowledge, out: renderMarkdown(text, m.width-12)})
	}
	return m, nil
}

// runLogItem runs the selected entry: the error grouping, a saved search
// (asking for its parameters), the builder or the question to Claude.
func (m Model) runLogItem() (tea.Model, tea.Cmd) {
	items := m.logItems()
	if m.logs.cursor >= len(items) {
		return m, nil
	}
	q := items[m.logs.cursor]
	name, pl := m.logProject()
	switch q.Name {
	case itemGroups:
		return m.runGroups("")
	case itemInvestigate:
		return m.investigateForm()
	case itemBuilder:
		return m.builderForm()
	case itemAsk:
		return m.askForm()
	}
	if strings.Contains(q.SPL, "{filter}") && pl.Filter == "" {
		m.msg = "chưa có filter prod cho " + name + " (logs_production.filter)"
		return m, nil
	}
	params := splunk.Placeholders(q.SPL)
	earliest, latest := m.window(q)
	run := func(vals map[string]string) (tea.Cmd, error) {
		vals["filter"] = pl.Filter
		spl, err := splunk.Expand(q.SPL, vals)
		if err != nil {
			return nil, err
		}
		if err := splunk.Validate(spl); err != nil {
			return nil, err
		}
		return m.searchCmd(spl, earliest, latest, false), nil
	}
	if len(params) == 0 {
		cmd, err := run(map[string]string{})
		if err != nil {
			m.msg = err.Error()
			return m, nil
		}
		m.logs.running = true
		m.setLogsContent(dimStyle.Render("Đang tìm…"))
		return m, cmd
	}
	f := newForm(q.Name, func(v map[string]string) (tea.Cmd, error) { return run(v) })
	f.hint = fmt.Sprintf("%s · %s", name, splunk.Describe(earliest, latest))
	for _, p := range params {
		if p == "service" && len(pl.Services) > 0 {
			f.addSelect(p, "Service (container)", m.serviceOptions(false), "")
			continue
		}
		f.addText(p, p, "", true)
	}
	m.modal = f
	return m, f.start()
}

// serviceOptions lists the project's containers, optionally with "all".
func (m Model) serviceOptions(withAll bool) func(*form) []option {
	_, pl := m.logProject()
	return func(*form) []option {
		var o []option
		if withAll {
			o = append(o, option{Label: "tất cả service", Value: ""})
		}
		if pl != nil {
			for _, s := range pl.Services {
				o = append(o, option{Label: s, Value: s})
			}
		}
		return o
	}
}

// formWindow is the preset preselected in a form: the current one, or 1 hour.
func (m Model) formWindow() string {
	p := splunk.Presets[1]
	if m.logs.preset > 0 {
		p = splunk.Presets[m.logs.preset-1]
	}
	return p.Earliest + "|" + p.Latest
}

// scoped adds the project filter and validates.
func (m Model) scoped(spl string) (string, error) {
	_, pl := m.logProject()
	if err := splunk.Validate(spl); err != nil {
		return "", err
	}
	if pl != nil && pl.Filter != "" {
		spl = splunk.Scope(pl.Filter, spl)
	}
	return spl, nil
}

// builderForm writes the SPL from a few fields (or takes a hand-written one).
func (m Model) builderForm() (tea.Model, tea.Cmd) {
	name, pl := m.logProject()
	f := newForm("Tìm log production — "+name, func(v map[string]string) (tea.Cmd, error) {
		spl := strings.TrimSpace(v["spl"])
		if spl == "" {
			spl = splunk.BuildSPL(splunk.Query{Service: v["service"], Level: v["level"], Text: v["text"],
				RequestID: v["request_id"], NoNoise: v["noise"] == "drop"})
		}
		spl, err := m.scoped(spl)
		if err != nil {
			return nil, err
		}
		e, l := splitWindow(v["window"])
		return m.searchCmd(spl, e, l, false), nil
	})
	f.hint = "SPL được tự sinh từ các ô; bộ lọc prod của project được thêm vào đầu. Chỉ đọc. Bấm c sau khi chạy để copy SPL."
	if pl.Filter == "" {
		f.hint = "Project chưa có filter prod: truy vấn chạy trên mọi log bạn xem được."
	}
	f.addSelect("service", "Service", m.serviceOptions(true), "")
	f.addSelect("level", "Mức", func(*form) []option {
		return []option{{"error trở lên", splunk.LevelError}, {"warn + error", splunk.LevelWarn}, {"mọi mức", splunk.LevelAny}}
	}, splunk.LevelError)
	f.addText("text", "Chứa chuỗi (mã giao dịch, path API, thông điệp…)", "", false)
	f.addText("request_id", "request_id", "", false)
	f.addSelect("noise", "Log không theo format (GORM SQL, fmt.Print…)", func(*form) []option {
		return []option{{"bỏ qua", "drop"}, {"giữ lại", "keep"}}
	}, "drop")
	f.addSelect("window", "Thời gian", presetOptions, m.formWindow())
	f.addArea("spl", "SPL tự viết (để trống = dùng các ô trên)", "", false)
	m.modal = f
	return m, f.start()
}

// askForm sends a question to Claude, which writes the SPL.
func (m Model) askForm() (tea.Model, tea.Cmd) {
	if m.d.SPLGen == nil {
		m.msg = "chưa cấu hình Claude để sinh SPL"
		return m, nil
	}
	if m.logs.generating {
		m.msg = "Claude đang viết SPL…"
		return m, nil
	}
	name, pl := m.logProject()
	gen := m.d.SPLGen
	lc := splgen.Context{Project: name, Filter: pl.Filter, Services: pl.Services, Examples: pl.Queries}
	if pl.Knowledge != "" {
		if b, err := plugin.Read("skills/prod-logs/" + pl.Knowledge); err == nil {
			lc.Knowledge = string(b)
		}
	}
	f := newForm("Hỏi log production — "+name, func(v map[string]string) (tea.Cmd, error) {
		q := strings.TrimSpace(v["question"])
		return tea.Sequence(
			func() tea.Msg { return splGenStartedMsg{} },
			func() tea.Msg {
				res, err := gen(context.Background(), lc, q)
				return splGenMsg{question: q, res: res, err: err}
			}), nil
	})
	f.hint = "Claude (không có tool) đọc tài liệu log của dự án rồi viết SPL; bạn xem và sửa trước khi chạy."
	f.addArea("question", "Bạn muốn tìm gì?", "", true)
	m.modal = f
	return m, f.start()
}

func (m Model) onSPLGen(msg splGenMsg) (tea.Model, tea.Cmd) {
	m.logs.generating = false
	if msg.err != nil {
		m.msg = "sinh SPL: " + msg.err.Error()
		return m, nil
	}
	if m.modal != nil {
		m.msg = "Claude đã viết xong SPL nhưng đang có hộp thoại khác mở"
		return m, nil
	}
	name, _ := m.logProject()
	res := msg.res
	f := newForm("SPL cho: "+truncate(msg.question, 60), func(v map[string]string) (tea.Cmd, error) {
		r := &splgen.Result{SPL: v["spl"], Earliest: v["earliest"], Latest: v["latest"]}
		if err := splgen.Check(r); err != nil {
			return nil, err
		}
		spl, err := m.scoped(r.SPL)
		if err != nil {
			return nil, err
		}
		return m.searchCmd(spl, r.Earliest, r.Latest, false), nil
	})
	f.hint = name + " · " + res.Explanation + "\nBộ lọc prod được thêm khi chạy. Thời gian dạng -4h, @d (hôm nay), -1d@d → @d (hôm qua)."
	f.addArea("spl", "SPL", res.SPL, true)
	f.addText("earliest", "Từ", res.Earliest, true)
	f.addText("latest", "Đến (trống = bây giờ)", res.Latest, false)
	m.modal = f
	m.msg = "xem lại SPL rồi Ctrl+S để chạy"
	return m, f.start()
}

// runGroups counts the project's errors by service and message.
func (m Model) runGroups(service string) (tea.Model, tea.Cmd) {
	spl, err := m.scoped(splunk.GroupSPL(service, groupsLimit))
	if err != nil {
		m.msg = err.Error()
		return m, nil
	}
	e, l := m.window(config.LogQuery{Earliest: "-24h"})
	return m.runLogSearch(spl, e, l, true)
}

func (m Model) runLogSearch(spl, earliest, latest string, groups bool) (tea.Model, tea.Cmd) {
	m.logs.running = true
	m.setLogsContent(dimStyle.Render("Đang tìm…"))
	return m, m.searchCmd(spl, earliest, latest, groups)
}

func (m Model) searchCmd(spl, earliest, latest string, groups bool) tea.Cmd {
	get := m.d.Splunk
	timeout := m.d.Config.Splunk.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return func() tea.Msg {
		msg := logsResultMsg{spl: spl, earliest: earliest, latest: latest, groups: groups}
		if get == nil {
			msg.err = errors.New("splunk chưa được cấu hình")
			return msg
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		start := time.Now()
		c, err := get(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.events, msg.err = c.Search(ctx, splunk.Request{SPL: spl, Earliest: earliest, Latest: latest, MaxEvents: logsMaxEvents})
		msg.took = time.Since(start)
		return msg
	}
}

func (m Model) onLogsResult(msg logsResultMsg) (tea.Model, tea.Cmd) {
	m.logs.running = false
	m.logs.lastSPL, m.logs.lastEarliest, m.logs.lastLatest, m.logs.lastGroups = msg.spl, msg.earliest, msg.latest, msg.groups
	w := max(20, m.logs.vp.Width)
	window := splunk.Describe(msg.earliest, msg.latest)
	if msg.err != nil {
		var b strings.Builder
		b.WriteString(titleStyle.Render(wrap(msg.spl, w)) + "\n")
		b.WriteString(dimStyle.Render(window) + "\n\n" + errStyle.Render(wrap(msg.err.Error(), w)) + "\n")
		if errors.Is(msg.err, splunk.ErrUnauthorized) || errors.Is(msg.err, splunk.ErrNoCredentials) {
			b.WriteString("\n" + warnStyle.Render("→ tab 4 Status, phím s để đăng nhập Splunk") + "\n")
		}
		m.logs.groupsFocus = false
		m.setLogsContent(b.String())
		return m, nil
	}
	if msg.groups {
		m.logs.groups = splunk.ParseGroups(msg.events)
		m.logs.groupCursor = 0
		m.logs.groupsFocus = len(m.logs.groups) > 0
		m.renderGroups()
		m.logs.vp.GotoTop()
		m.msg = ""
		return m, nil
	}
	m.logs.groupsFocus = false
	m.logs.lastEvents = msg.events
	var b strings.Builder
	b.WriteString(titleStyle.Render(wrap(msg.spl, w)) + "\n")
	note := ""
	if len(msg.events) == logsMaxEvents {
		note = fmt.Sprintf(" (đã đạt giới hạn %d — thu hẹp truy vấn hoặc bấm o để xem trên Splunk)", logsMaxEvents)
	}
	back := ""
	if len(m.logs.groups) > 0 {
		back = " · g về nhóm lỗi"
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("%s · %d dòng · %.1fs%s · n tạo task điều tra · o mở Splunk · c copy SPL · r chạy lại%s", window, len(msg.events), msg.took.Seconds(), note, back)) + "\n\n")
	if len(msg.events) == 0 {
		b.WriteString(dimStyle.Render("Không có kết quả."))
	}
	if splunk.IsTransforming(msg.spl) {
		for _, l := range splunk.Table(msg.events, w) {
			b.WriteString(l + "\n")
		}
	} else {
		for _, l := range splunk.Pretty(msg.events, w) {
			switch {
			case strings.Contains(l, " ERROR ") || strings.Contains(l, " FATAL ") || strings.Contains(l, " PANIC "):
				b.WriteString(errStyle.Render(l) + "\n")
			case strings.Contains(l, " WARN "):
				b.WriteString(warnStyle.Render(l) + "\n")
			default:
				b.WriteString(l + "\n")
			}
		}
	}
	m.setLogsContent(b.String())
	m.msg = ""
	return m, nil
}

// renderGroups draws the error groups with the selected one detailed, and
// keeps the selected row in view.
func (m *Model) renderGroups() {
	w := max(30, m.logs.vp.Width)
	name, _ := m.logProject()
	gs := m.logs.groups
	var b strings.Builder
	total := 0
	for _, g := range gs {
		total += g.Count
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("Nhóm lỗi — %s · %s", name, splunk.Describe(m.logs.lastEarliest, m.logs.lastLatest))) + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d nhóm · %d lỗi · ↑↓ chọn · Enter xem log · n tạo task điều tra · f tạo task sửa ngay · s chỉ service này · esc về danh sách", len(gs), total)) + "\n\n")
	if len(gs) == 0 {
		b.WriteString(dimStyle.Render("Không có lỗi nào trong khoảng thời gian này."))
	}
	selLine := 0
	for i, g := range gs {
		last := "     "
		if !g.Last.IsZero() {
			last = g.Last.Local().Format("15:04")
		}
		row := fmt.Sprintf("%6d  %-16s %s  %s", g.Count, truncate(g.Service, 16), last, g.Signature)
		row = truncate(row, w)
		if i == m.logs.groupCursor && m.logs.groupsFocus {
			selLine = strings.Count(b.String(), "\n")
			b.WriteString(selStyle.Render(row) + "\n")
		} else {
			b.WriteString(row + "\n")
		}
	}
	if g := m.currentGroup(); g != nil {
		b.WriteString("\n" + accentStyle.Render(strings.Repeat("─", min(w, 40))) + "\n")
		fmt.Fprintf(&b, "%s %s\n", titleStyle.Render(g.Service), dimStyle.Render(fmt.Sprintf("%d lần · %s → %s", g.Count, g.First.Local().Format("02/01 15:04"), g.Last.Local().Format("02/01 15:04"))))
		b.WriteString(wrap(g.Sample, w) + "\n")
		for _, kv := range [][2]string{{"caller", g.Caller}, {"prefix", g.Prefix}, {"request_id", g.RequestID}} {
			if kv[1] != "" {
				b.WriteString(dimStyle.Render(kv[0]+": ") + kv[1] + "\n")
			}
		}
		if st := strings.TrimSpace(g.Stacktrace); st != "" {
			lines := strings.Split(st, "\n")
			b.WriteString(dimStyle.Render("stacktrace:") + "\n" + dimStyle.Render(strings.Join(lines[:min(6, len(lines))], "\n")) + "\n")
		}
	}
	m.logs.intro = false
	m.logs.vpContent = b.String()
	m.logs.vp.SetContent(m.logs.vpContent)
	if h := m.logs.vp.Height; selLine < m.logs.vp.YOffset || selLine >= m.logs.vp.YOffset+h-1 {
		m.logs.vp.SetYOffset(max(0, selLine-h/3))
	}
}

func (m Model) currentGroup() *splunk.ErrorGroup {
	if m.logs.groupCursor < 0 || m.logs.groupCursor >= len(m.logs.groups) {
		return nil
	}
	return &m.logs.groups[m.logs.groupCursor]
}

// groupsKey handles the keys of the error group list; ok=false lets the key
// through to the normal handling.
func (m Model) groupsKey(k tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch k.String() {
	case "j", "down":
		m.logs.groupCursor = min(m.logs.groupCursor+1, max(0, len(m.logs.groups)-1))
	case "k", "up":
		m.logs.groupCursor = max(0, m.logs.groupCursor-1)
	case "home", "g":
		m.logs.groupCursor = 0
	case "end", "G":
		m.logs.groupCursor = max(0, len(m.logs.groups)-1)
	case "esc":
		m.logs.groupsFocus = false
		m.renderGroups()
		return m, nil, true
	case "enter":
		g := m.currentGroup()
		if g == nil {
			return m, nil, true
		}
		m.logs.groupsFocus = false // the drill result replaces the pane; g comes back
		spl, err := m.scoped(splunk.DrillSPL(*g))
		if err != nil {
			m.msg = err.Error()
			return m, nil, true
		}
		mm, cmd := m.runLogSearch(spl, m.logs.lastEarliest, m.logs.lastLatest, false)
		return mm.(Model), cmd, true
	case "s":
		g := m.currentGroup()
		if g == nil {
			return m, nil, true
		}
		mm, cmd := m.runGroups(g.Service)
		return mm.(Model), cmd, true
	case "n":
		g := m.currentGroup()
		if g == nil {
			return m, nil, true
		}
		mm, cmd := m.investigateFromGroup(*g)
		return mm.(Model), cmd, true
	case "f":
		mm, cmd := m.taskFromGroup()
		return mm.(Model), cmd, true
	default:
		return m, nil, false
	}
	m.renderGroups()
	return m, nil, true
}

// repoForService maps a container to the project repo of the same name
// (payment-auth ↔ payment_auth); "" when none matches.
func (m Model) repoForService(project, service string) string {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
	for _, r := range m.d.Config.Projects[project].RepoNames() {
		if norm(r) == norm(service) {
			return r
		}
	}
	return ""
}

// taskFromGroup opens the new-task form prefilled to fix the selected error
// group: project, the service's repo, a title and a description with the
// evidence and the search to find it again.
func (m Model) taskFromGroup() (tea.Model, tea.Cmd) {
	g := m.currentGroup()
	if g == nil {
		return m, nil
	}
	name, pl := m.logProject()
	window := splunk.Describe(m.logs.lastEarliest, m.logs.lastLatest)
	title := "Sửa lỗi prod " + g.Service + ": " + truncate(strings.TrimSpace(g.Signature), 70)
	t := source.Ticket{Project: name, Repo: m.repoForService(name, g.Service), Title: title, Body: splunk.TicketBody(name, *g, pl.Filter, window)}
	hint := fmt.Sprintf("Từ nhóm lỗi %s (%d lần, %s). Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S.", g.Service, g.Count, window)
	if t.Repo == "" {
		hint += " Không tìm thấy repo trùng tên service: chọn repo bắt đầu hoặc để agent tự chọn."
	}
	return m.newTaskForm(t, hint)
}

func (m *Model) setLogsContent(s string) {
	m.logs.intro = false
	m.logs.vp.SetContent(s)
	m.logs.vpContent = s
	m.logs.vp.GotoTop()
}

// renderMarkdown formats Markdown for the terminal, falling back to plain text.
func renderMarkdown(md string, width int) string {
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(max(40, width)))
	if err != nil {
		return md
	}
	out, err := r.Render(md)
	if err != nil {
		return md
	}
	return out
}
