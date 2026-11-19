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

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/plugin"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splgen"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// logsMaxEvents caps one search from the TUI when the config sets no
// splunk.max_events.
const logsMaxEvents = 200

// logsLimit is the TUI's cap per search: 0 (no limit) when splunk.max_events
// is 0, else at least logsMaxEvents.
func logsLimit(c *config.Config) int {
	if c != nil && c.Splunk.MaxEvents == 0 && c.Path != "" {
		return 0
	}
	if c != nil && c.Splunk.MaxEvents > logsMaxEvents {
		return c.Splunk.MaxEvents
	}
	return logsMaxEvents
}

// groupsLimit caps the error groups of one grouping search.
const groupsLimit = 60

type (
	logsResultMsg struct {
		spl, earliest, latest string
		groups                bool // the search is the error grouping (rows are groups)
		events                []splunk.Event
		took                  time.Duration
		limit                 int // events cap of the search (0: none)
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
	envProd logsEnv = iota // the default: Production comes first
	envQC
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

	// Fix suggestions of the scheduled scans (logs_suggest.go).
	suggestions int // waiting, every project (tab label; refreshed each tick)
	flows       []*store.ErrorFlow
	flowOpen    map[int64]bool // expanded flows
	suggCursor  int            // in suggEntries()
	suggFocus   bool
	scanning    string // project being scanned now (key r), "" when none
	suggHidden  bool   // the list shows the hidden suggestions (key v)

	rec recState // debugging recipes (logs_recipe.go)
}

// Special entries of the saved-search list.
const (
	itemGroups      = "Nhóm lỗi theo service"
	itemInvestigate = "Điều tra sự cố…"
	itemBuilder     = "Tìm theo điều kiện…"
	itemAsk         = "Hỏi bằng lời (Claude viết SPL)…"
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

// logItems are the shared actions (grouping, investigation, search), the
// question to Claude, then the project's saved searches.
func (m Model) logItems() []config.LogQuery {
	_, pl := m.logProject()
	if pl == nil {
		return nil
	}
	var items []config.LogQuery
	if name, _ := m.logProject(); m.hasScan(name) {
		items = append(items, config.LogQuery{Name: itemSuggest, Note: "Lỗi production taskpilot tự quét theo lịch (logs_production.scan), đã gom nhóm: tạo task điều tra / sửa, hoặc bỏ qua."})
	}
	items = append(items, []config.LogQuery{
		{Name: itemGroups, Note: "Gom lỗi giống nhau (số, mã, UUID được che) của mọi service; chọn một nhóm để xem log, điều tra hoặc tạo task sửa."},
		{Name: itemInvestigate, Note: "Khách / vận hành báo lỗi? Mô tả triệu chứng, mã đơn/giao dịch, khoảng thời gian: tạo task để agent điều tra nguyên nhân gốc trên Splunk, code và DB QC, báo cáo trước khi sửa."},
		{Name: itemBuilder, Note: "Chọn service, mức, chuỗi cần tìm, request_id: taskpilot tự viết SPL."},
		{Name: itemAsk, Note: "Mô tả bằng lời (vd \"refund TCB lỗi sáng nay\"); Claude đọc tài liệu log của dự án và viết SPL để bạn sửa trước khi chạy."},
	}...)
	items = append(items, config.LogQuery{Name: itemRecipes, Note: "Những bước tra cứu bạn hay lặp lại khi debug (DB QC, Splunk, log QC…), chạy ngay không tốn Claude; agent cũng dùng được. R: Claude đọc các task gần đây và đề xuất quy trình mới."})
	return append(items, pl.Queries...)
}

// prodRows lays out the Production list for logsLeft.
func (m Model) prodRows() []logRow {
	name, pl := m.logProject()
	if pl == nil {
		return nil
	}
	var rows []logRow
	items := m.logItems()
	actions := len(items) - len(pl.Queries)
	rows = append(rows, logRow{plain: "── Công cụ", item: -1})
	for i, q := range items {
		if i == actions && actions < len(items) {
			rows = append(rows, logRow{plain: "", item: -1}, logRow{plain: "── Truy vấn có sẵn", item: -1})
		}
		if q.Name == itemSuggest {
			n := m.suggestionCount(name)
			plain := fmt.Sprintf("%s (%d)", q.Name, n)
			label := q.Name
			if n > 0 {
				label += " " + warnStyle.Render(fmt.Sprintf("%d", n))
			}
			rows = append(rows, logRow{plain: plain, label: label, note: q.Note, item: i})
			continue
		}
		if q.Name == itemRecipes {
			n, fresh := m.recipeCounts(name)
			plain := fmt.Sprintf("%s (%d)", q.Name, n)
			label := plain
			if fresh > 0 {
				plain += fmt.Sprintf(" · %d mới", fresh)
				label += " " + warnStyle.Render(fmt.Sprintf("· %d mới", fresh))
			}
			rows = append(rows, logRow{plain: plain, label: label, note: q.Note, item: i})
			continue
		}
		note := q.Note
		if q.SPL != "" {
			note = strings.TrimSpace(note + "\nSPL: " + q.SPL)
		}
		rows = append(rows, logRow{plain: q.Name, note: note, item: i})
	}
	return rows
}

func itemIndex(items []config.LogQuery, name string) int {
	for i, q := range items {
		if q.Name == name {
			return i
		}
	}
	return 0
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
	prod, qc := tabStyle.Render("Production · Splunk"), activeTabStyle.Render("QC · Argo CD")
	if m.logs.env != envQC {
		prod, qc = activeTabStyle.Render("Production · Splunk"), tabStyle.Render("QC · Argo CD")
	}
	return " " + prod + "   " + qc + " " + dimStyle.Render("  e")
}

func (m Model) logsView() string {
	if m.logs.env == envQC {
		return m.qcView()
	}
	return m.prodView()
}

func (m Model) logsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "e":
		if !m.logs.groupsFocus && !m.logs.qc.groupsFocus {
			return m.switchEnv()
		}
	case "t", "T": // one time window for both environments
		n := len(splunk.Presets) + 1
		if k.String() == "t" {
			m.logs.preset = (m.logs.preset + 1) % n
		} else {
			m.logs.preset = (m.logs.preset + n - 1) % n
		}
		m.msg = "thời gian: " + m.presetLabel() + " (áp dụng cho lần đọc tiếp theo; r để chạy lại)"
		return m, nil
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
	name, pl := m.logProject()
	if pl == nil {
		var b strings.Builder
		b.WriteString(m.envLine() + "\n" + dimStyle.Render("Chưa có project nào có logs_production trong config."))
		return boxStyle.Width(m.width - 2).Height(m.innerHeight()).Render(b.String())
	}
	extra := ""
	if pl.Filter == "" {
		extra = quote(warnStyle.Render(wrap(fmt.Sprintf("Chưa có filter prod cho %s: đặt logs_production.filter trong config.", name), m.listWidth()-4)), cAccent)
	}
	ps := m.logProjects()
	return m.logsLeft(ps, m.logs.proj%len(ps), m.prodRows(), m.logs.cursor, m.logs.groupsFocus, extra)
}

func (m Model) prodKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.logs.rec.focus {
		if mm, cmd, ok := m.recipeKey(k); ok {
			return mm, cmd
		}
	}
	if m.logs.suggFocus {
		if mm, cmd, ok := m.suggestKey(k); ok {
			return mm, cmd
		}
	}
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
	case "pgdown", "ctrl+d", " ":
		m.logs.vp.HalfPageDown()
	case "pgup", "ctrl+u":
		m.logs.vp.HalfPageUp()
	case "enter":
		return m.runLogItem()
	case "/":
		m.logs.cursor = itemIndex(items, itemBuilder)
		return m.runLogItem()
	case "a":
		m.logs.cursor = itemIndex(items, itemAsk)
		return m.runLogItem()
	case "g":
		if len(m.logs.groups) > 0 { // back to the last groups; r or Enter on the entry searches again
			m.logs.groupsFocus = true
			m.renderGroups()
			return m, nil
		}
		m.logs.cursor = itemIndex(items, itemGroups)
		return m.runLogItem()
	case "r":
		if m.logs.cursor < len(items) && items[m.logs.cursor].Name == itemSuggest {
			return m.scanNow()
		}
		if m.logs.lastSPL != "" {
			e, l := m.logs.lastEarliest, m.logs.lastLatest
			if m.logs.preset > 0 { // a new preset applies to the rerun
				p := splunk.Presets[m.logs.preset-1]
				e, l = p.Earliest, p.Latest
			}
			return m.runLogSearch(m.logs.lastSPL, e, l, m.logs.lastGroups)
		}
	case "v":
		if m.logs.cursor < len(items) && items[m.logs.cursor].Name == itemSuggest {
			mm, _ := m.openSuggestions()
			m = mm.(Model)
			mm2, cmd, _ := m.suggestKey(k)
			return mm2, cmd
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
	case "R":
		if m.logs.cursor < len(items) && items[m.logs.cursor].Name == itemRecipes {
			return m.confirmReflect(), nil
		}
	case "c":
		if run := m.logs.rec.shown; run != nil {
			if err := m.d.Copy(run.out); err != nil {
				m.msg = "copy: " + err.Error()
			} else {
				m.msg = "đã copy kết quả quy trình"
			}
			break
		}
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
		if run := m.logs.rec.shown; run != nil {
			return m.investigateFromRecipe(run)
		}
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
	case itemSuggest:
		return m.openSuggestions()
	case itemRecipes:
		return m.openRecipes()
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
		if err := splgen.Check(r, m.d.Config.Splunk.MaxLookback); err != nil {
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
	limit := logsLimit(m.d.Config)
	timeout := m.d.Config.Splunk.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return func() tea.Msg {
		msg := logsResultMsg{spl: spl, earliest: earliest, latest: latest, groups: groups, limit: limit}
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
		msg.events, msg.err = c.Search(ctx, splunk.Request{SPL: spl, Earliest: earliest, Latest: latest, MaxEvents: limit})
		msg.took = time.Since(start)
		return msg
	}
}

func (m Model) onLogsResult(msg logsResultMsg) (tea.Model, tea.Cmd) {
	m.logs.running = false
	m.logs.rec.shown = nil
	m.logs.lastSPL, m.logs.lastEarliest, m.logs.lastLatest, m.logs.lastGroups = msg.spl, msg.earliest, msg.latest, msg.groups
	w := max(20, m.logs.vp.Width)
	window := splunk.Describe(msg.earliest, msg.latest)
	if msg.err != nil {
		var b strings.Builder
		b.WriteString(section("✗ Lỗi", "", w) + "\n")
		b.WriteString(quote(accentStyle.Render(wrap(msg.spl, w-2)), cRule) + "\n")
		b.WriteString(dimStyle.Render("◷ "+window) + "\n\n" + quote(errStyle.Render(wrap(msg.err.Error(), w-2)), cErr) + "\n")
		if errors.Is(msg.err, splunk.ErrUnauthorized) || errors.Is(msg.err, splunk.ErrNoCredentials) {
			b.WriteString("\n" + warnStyle.Render("→ tab 4 Status, phím ") + keyCap("s") + warnStyle.Render(" để đăng nhập Splunk") + "\n")
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
	keys := "n điều tra · o Splunk Web · c copy · r chạy lại"
	if len(m.logs.groups) > 0 {
		keys += " · g về nhóm lỗi"
	}
	renderResultHeader(&b, msg.spl, window, len(msg.events), msg.took, msg.limit > 0 && len(msg.events) == msg.limit, keys, w)
	if splunk.IsTransforming(msg.spl) {
		for _, l := range splunk.Table(msg.events, w) {
			b.WriteString(l + "\n")
		}
	} else {
		for _, l := range splunk.Pretty(msg.events, w) {
			b.WriteString(colorLogLine(l) + "\n")
		}
	}
	m.setLogsContent(b.String())
	m.msg = ""
	return m, nil
}

// renderGroups draws the error groups (shared layout) and keeps the
// selected row in view.
func (m *Model) renderGroups() {
	w := max(30, m.logs.vp.Width)
	name, _ := m.logProject()
	var b strings.Builder
	title := fmt.Sprintf("Nhóm lỗi Production — %s · %s", name, splunk.Describe(m.logs.lastEarliest, m.logs.lastLatest))
	selLine := renderGroupList(&b, title, m.logs.groups, m.logs.groupCursor, m.logs.groupsFocus, w, groupKeys)
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
	return m.fixFlowTask(*g, "", nil)
}

// fixFlowTask opens the fix task form of an error group and, when it belongs
// to a failing flow, the other groups of that flow: one task for the path.
func (m Model) fixFlowTask(gv splunk.ErrorGroup, flow string, related []splunk.ErrorGroup, after ...func(string)) (tea.Model, tea.Cmd) {
	g := &gv
	name, pl := m.logProject()
	window := splunk.Describe(m.logs.lastEarliest, m.logs.lastLatest)
	title := "Sửa lỗi prod " + g.Service + ": " + truncate(strings.TrimSpace(g.Signature), 70)
	extra := ""
	if flow != "" || len(related) > 0 {
		title = "Sửa luồng lỗi prod: " + truncate(flow, 80)
		extra = splunk.FlowSection(flow, related)
	}
	t := source.Ticket{Project: name, Repo: m.repoForService(name, g.Service), Title: title, Body: splunk.FlowTicketBody(name, *g, pl.Filter, window, extra)}
	hint := fmt.Sprintf("Từ nhóm lỗi %s (%d lần, %s). Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S.", g.Service, g.Count, window)
	if t.Repo == "" {
		hint += " Không tìm thấy repo trùng tên service: chọn repo bắt đầu hoặc để agent tự chọn."
	}
	return m.newTaskForm(t, hint, after...)
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
