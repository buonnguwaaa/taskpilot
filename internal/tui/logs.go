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
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// logsMaxEvents caps one search from the TUI.
const logsMaxEvents = 200

var logRanges = []string{"", "-15m", "-1h", "-4h", "-24h", "-7d"}

type logsResultMsg struct {
	spl, earliest string
	events        []splunk.Event
	took          time.Duration
	err           error
}

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

	proj     int
	cursor   int
	rangeIdx int
	running  bool
	vp       viewport.Model

	lastSPL, lastEarliest string
	intro                 bool // the help text is shown until the first search
	vpContent             string
}

func (m Model) logProjects() []string { return m.d.Config.LogProjects() }

func (m Model) logProject() (string, *config.ProdLogs) {
	ps := m.logProjects()
	if len(ps) == 0 {
		return "", nil
	}
	name := ps[m.logs.proj%len(ps)]
	return name, m.d.Config.Projects[name].LogsProduction
}

// logItems are the saved searches plus the free-search entry.
func (m Model) logItems() []config.LogQuery {
	_, pl := m.logProject()
	if pl == nil {
		return nil
	}
	return append(append([]config.LogQuery(nil), pl.Queries...), config.LogQuery{Name: "✎ Tìm tự do…"})
}

func (m Model) effectiveEarliest(q config.LogQuery) string {
	if r := logRanges[m.logs.rangeIdx]; r != "" {
		return r
	}
	if q.Earliest != "" {
		return q.Earliest
	}
	return m.d.Config.Splunk.DefaultEarliest
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
	if k.String() == "e" {
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
	rng := logRanges[m.logs.rangeIdx]
	if rng == "" {
		rng = "mặc định"
	}
	b.WriteString(strings.Join(tabs, " ") + dimStyle.Render("  ←/→ · thời gian: "+rng) + "\n")
	items := m.logItems()
	rows := m.listRows() - 5
	start := 0
	if m.logs.cursor >= rows {
		start = m.logs.cursor - rows + 1
	}
	for i := start; i < len(items) && i-start < rows; i++ {
		label := truncate(items[i].Name, lw-4)
		if i == m.logs.cursor {
			b.WriteString(selStyle.Render(" "+label+" ") + "\n")
		} else {
			b.WriteString(" " + label + "\n")
		}
	}
	if m.logs.cursor < len(items) {
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
		m.logs.rangeIdx = (m.logs.rangeIdx + 1) % len(logRanges)
	case "pgdown", "ctrl+d", " ":
		m.logs.vp.HalfViewDown()
	case "pgup", "ctrl+u":
		m.logs.vp.HalfViewUp()
	case "enter":
		return m.runLogItem()
	case "/":
		m.logs.cursor = len(items) - 1
		return m.runLogItem()
	case "r":
		if m.logs.lastSPL != "" {
			return m.runLogSearch(m.logs.lastSPL, m.logs.lastEarliest)
		}
	case "o":
		if m.logs.lastSPL == "" {
			m.msg = "chưa chạy truy vấn nào"
			break
		}
		if err := m.d.OpenURL(splunk.WebURL(m.d.Config.Splunk.URL, m.d.Config.Splunk.Locale, m.logs.lastSPL, m.logs.lastEarliest)); err != nil {
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

// runLogItem runs the selected saved search, asking for its parameters first.
func (m Model) runLogItem() (tea.Model, tea.Cmd) {
	items := m.logItems()
	if m.logs.cursor >= len(items) {
		return m, nil
	}
	q := items[m.logs.cursor]
	name, pl := m.logProject()
	if q.SPL == "" { // free search
		f := newForm("Tìm log production — "+name, func(v map[string]string) (tea.Cmd, error) {
			if err := splunk.Validate(v["spl"]); err != nil {
				return nil, err
			}
			spl := v["spl"]
			if pl.Filter != "" {
				spl = splunk.Scope(pl.Filter, spl)
			}
			return m.searchCmd(spl, v["earliest"]), nil
		})
		f.hint = "Bộ lọc prod của project được tự thêm vào đầu. Chỉ đọc: không dùng outputlookup/collect/delete…"
		if pl.Filter == "" {
			f.hint = "Project chưa có filter prod: truy vấn chạy trên mọi log bạn xem được."
		}
		f.addArea("spl", "SPL", "level=error", true)
		f.addSelect("earliest", "Từ", func(*form) []option {
			return []option{{"15 phút", "-15m"}, {"1 giờ", "-1h"}, {"4 giờ", "-4h"}, {"24 giờ", "-24h"}, {"7 ngày", "-7d"}}
		}, "-1h")
		m.modal = f
		return m, f.start()
	}
	if strings.Contains(q.SPL, "{filter}") && pl.Filter == "" {
		m.msg = "chưa có filter prod cho " + name + " (logs_production.filter)"
		return m, nil
	}
	params := splunk.Placeholders(q.SPL)
	earliest := m.effectiveEarliest(q)
	run := func(vals map[string]string) (tea.Cmd, error) {
		vals["filter"] = pl.Filter
		spl, err := splunk.Expand(q.SPL, vals)
		if err != nil {
			return nil, err
		}
		if err := splunk.Validate(spl); err != nil {
			return nil, err
		}
		return m.searchCmd(spl, earliest), nil
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
	f.hint = fmt.Sprintf("%s · từ %s", name, earliest)
	for _, p := range params {
		if p == "service" && len(pl.Services) > 0 {
			services := pl.Services
			f.addSelect(p, "Service (container)", func(*form) []option {
				var o []option
				for _, s := range services {
					o = append(o, option{s, s})
				}
				return o
			}, "")
			continue
		}
		f.addText(p, p, "", true)
	}
	m.modal = f
	return m, f.start()
}

func (m Model) runLogSearch(spl, earliest string) (tea.Model, tea.Cmd) {
	m.logs.running = true
	m.setLogsContent(dimStyle.Render("Đang tìm…"))
	return m, m.searchCmd(spl, earliest)
}

func (m Model) searchCmd(spl, earliest string) tea.Cmd {
	get := m.d.Splunk
	timeout := m.d.Config.Splunk.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return func() tea.Msg {
		if get == nil {
			return logsResultMsg{spl: spl, earliest: earliest, err: errors.New("splunk chưa được cấu hình")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		start := time.Now()
		c, err := get(ctx)
		if err != nil {
			return logsResultMsg{spl: spl, earliest: earliest, err: err}
		}
		ev, err := c.Search(ctx, splunk.Request{SPL: spl, Earliest: earliest, MaxEvents: logsMaxEvents})
		return logsResultMsg{spl: spl, earliest: earliest, events: ev, err: err, took: time.Since(start)}
	}
}

func (m Model) onLogsResult(msg logsResultMsg) (tea.Model, tea.Cmd) {
	m.logs.running = false
	m.logs.lastSPL, m.logs.lastEarliest = msg.spl, msg.earliest
	w := max(20, m.logs.vp.Width)
	var b strings.Builder
	b.WriteString(titleStyle.Render(wrap(msg.spl, w)) + "\n")
	if msg.err != nil {
		b.WriteString(dimStyle.Render("từ "+msg.earliest) + "\n\n" + errStyle.Render(wrap(msg.err.Error(), w)) + "\n")
		if errors.Is(msg.err, splunk.ErrUnauthorized) || errors.Is(msg.err, splunk.ErrNoCredentials) {
			b.WriteString("\n" + warnStyle.Render("→ tab 4 Status, phím s để đăng nhập Splunk") + "\n")
		}
		m.setLogsContent(b.String())
		return m, nil
	}
	note := ""
	if len(msg.events) == logsMaxEvents {
		note = fmt.Sprintf(" (đã đạt giới hạn %d — thu hẹp truy vấn hoặc bấm o để xem trên Splunk)", logsMaxEvents)
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("từ %s · %d event · %.1fs%s · o mở Splunk · c copy SPL · r chạy lại", msg.earliest, len(msg.events), msg.took.Seconds(), note)) + "\n\n")
	if len(msg.events) == 0 {
		b.WriteString(dimStyle.Render("Không có event nào."))
	}
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
	m.setLogsContent(b.String())
	m.msg = ""
	return m, nil
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
