package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Fix suggestions left by the scheduled production error scans
// (internal/scan), grouped by failing flow: the error groups logged in the
// same requests/transactions, or of one call chain, are one flow. A flow is
// one row (Enter/space opens it); n, f, x and z act on the whole flow, or on
// one group when the cursor is on it.

const itemSuggest = "Đề xuất sửa lỗi"

const (
	suggestKeys = "↑↓ chọn · Enter mở luồng / xem log · n điều tra · f tạo task sửa · x bỏ qua hẳn · z ẩn 7 ngày · v xem đã ẩn · r quét ngay · esc về danh sách"
	hiddenKeys  = "↑↓ chọn · Enter mở luồng / xem log · u bỏ ẩn · U bỏ ẩn tất cả · v về đề xuất · esc về danh sách"
)

// suggKeys are the keys of the suggestion list in its current view.
func (m Model) suggKeys() string {
	if m.logs.suggHidden {
		return hiddenKeys
	}
	return suggestKeys
}

// suggestionCount is the number of error groups waiting ("" = every project).
func (m Model) suggestionCount(project string) int {
	if m.d.Store == nil {
		return 0
	}
	gs, _ := m.d.Store.Suggestions(project)
	return len(gs)
}

// hasScan reports whether a project is scanned or has suggestions.
func (m Model) hasScan(project string) bool {
	p := m.d.Config.Projects[project]
	return p.LogsProduction != nil && p.LogsProduction.Scan != nil || m.suggestionCount(project) > 0
}

func suggestionGroup(s *store.Suggestion) splunk.ErrorGroup {
	return splunk.ErrorGroup{Service: s.Service, Signature: s.Signature, Count: s.LastCount, First: s.FirstSeen, Last: s.LastSeen,
		Sample: s.Sample, Caller: s.Caller, Prefix: s.Prefix, RequestID: s.RequestID, Stacktrace: s.Stacktrace}
}

// suggEntry is one selectable row: a flow (member nil) or one of its groups.
type suggEntry struct {
	flow   *store.ErrorFlow
	member *store.Suggestion
}

// multi reports whether a flow has more than one group (else it is shown as
// the group itself).
func multi(f *store.ErrorFlow) bool { return len(f.Members) > 1 }

func (m Model) suggEntries() []suggEntry {
	var out []suggEntry
	for _, f := range m.logs.flows {
		if !multi(f) {
			out = append(out, suggEntry{f, f.Members[0]})
			continue
		}
		out = append(out, suggEntry{flow: f})
		if m.logs.flowOpen[f.ID] {
			for _, s := range f.Members {
				out = append(out, suggEntry{f, s})
			}
		}
	}
	return out
}

func (m Model) currentEntry() *suggEntry {
	es := m.suggEntries()
	if m.logs.suggCursor < 0 || m.logs.suggCursor >= len(es) {
		return nil
	}
	return &es[m.logs.suggCursor]
}

// loadFlows reads the project's waiting flows.
func (m *Model) loadFlows() error {
	name, _ := m.logProject()
	load := m.d.Store.SuggestionFlows
	if m.logs.suggHidden {
		load = m.d.Store.HiddenFlows
	}
	fs, err := load(name)
	if err != nil {
		return err
	}
	m.logs.flows = fs
	if m.logs.flowOpen == nil {
		m.logs.flowOpen = map[int64]bool{}
	}
	n := len(m.suggEntries())
	m.logs.suggCursor = min(m.logs.suggCursor, max(0, n-1))
	m.logs.suggFocus = n > 0 || m.logs.suggHidden // the hidden view keeps the keys (v back)
	return nil
}

// openSuggestions shows the project's suggestions with the cursor in them.
func (m Model) openSuggestions() (tea.Model, tea.Cmd) {
	m.logs.suggCursor = 0
	m.logs.suggHidden = false
	if err := m.loadFlows(); err != nil {
		m.msg = "đọc đề xuất: " + err.Error()
		return m, nil
	}
	m.logs.groupsFocus = false
	m.logs.rec.focus, m.logs.rec.shown = false, nil
	m.renderSuggestions()
	m.logs.vp.GotoTop()
	return m, nil
}

func flowName(f *store.ErrorFlow) string {
	if f.Title != "" {
		return f.Title
	}
	return f.Members[0].Signature
}

func flowServices(f *store.ErrorFlow) string {
	var out []string
	seen := map[string]bool{}
	for _, s := range f.Members {
		if !seen[s.Service] {
			seen[s.Service] = true
			out = append(out, s.Service)
		}
	}
	return strings.Join(out, ", ")
}

func flowHow(f *store.ErrorFlow) string {
	switch f.How {
	case "request_id":
		return "cùng request/giao dịch"
	case "claude":
		return "cùng chuỗi gọi (Claude gom)"
	}
	return ""
}

func (m *Model) renderSuggestions() {
	w := max(30, m.logs.vp.Width)
	name, _ := m.logProject()
	var b strings.Builder
	title := "Đề xuất sửa lỗi — " + name
	if m.logs.suggHidden {
		title = "Đề xuất đã ẩn — " + name
	}
	if last, _ := m.d.Store.LastScan(name); last != nil {
		title += fmt.Sprintf(" · lần quét cuối %s (%s → %s)", last.StartedAt.Local().Format("02/01 15:04"),
			last.Earliest.Local().Format("02/01 15:04"), last.Latest.Local().Format("15:04"))
		if last.Error != "" {
			b.WriteString(warnStyle.Render(wrap("lần quét cuối lỗi: "+last.Error, w)) + "\n")
		}
	}
	if p := m.d.Config.Projects[name]; p.LogsProduction != nil && p.LogsProduction.Scan != nil {
		title += " · lịch " + strings.Join(p.LogsProduction.Scan.Times, ", ")
	}
	if m.logs.scanning == name {
		b.WriteString(accentStyle.Render("Đang quét lỗi production…") + "\n")
	}
	groups, total := 0, 0
	for _, f := range m.logs.flows {
		groups += len(f.Members)
		total += f.Total()
	}
	b.WriteString(titleStyle.Render(wrap(title, w)) + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("%d luồng · %d nhóm · %d lỗi · %s", len(m.logs.flows), groups, total, m.suggKeys())) + "\n\n")
	es := m.suggEntries()
	switch {
	case len(es) == 0 && m.logs.suggHidden:
		b.WriteString(dimStyle.Render("Không có đề xuất nào đang ẩn."))
	case len(es) == 0:
		b.WriteString(dimStyle.Render("Không có đề xuất nào đang chờ. v xem các đề xuất đã ẩn."))
	}
	sel := 0
	for i, e := range es {
		var row string
		switch {
		case e.member == nil:
			mark := "▸"
			if m.logs.flowOpen[e.flow.ID] {
				mark = "▾"
			}
			row = fmt.Sprintf("%s %6d  %s  (%d nhóm: %s)", mark, e.flow.Total(), flowName(e.flow), len(e.flow.Members), flowServices(e.flow))
		case multi(e.flow):
			row = fmt.Sprintf("    %6d  %s%-20s %s", e.member.LastCount, m.hiddenTag(e.member), truncate(e.member.Service, 20), e.member.Signature)
		default:
			row = fmt.Sprintf("  %6d  %s%-20s %s", e.member.LastCount, m.hiddenTag(e.member), truncate(e.member.Service, 20), e.member.Signature)
		}
		row = truncate(row, w)
		switch {
		case i == m.logs.suggCursor && m.logs.suggFocus:
			sel = strings.Count(b.String(), "\n")
			b.WriteString(selStyle.Render(row) + "\n")
		case e.member == nil:
			b.WriteString(accentStyle.Render(row) + "\n")
		default:
			b.WriteString(row + "\n")
		}
	}
	if e := m.currentEntry(); e != nil {
		b.WriteString("\n" + accentStyle.Render(strings.Repeat("─", min(w, 40))) + "\n")
		if e.member == nil {
			f := e.flow
			b.WriteString(titleStyle.Render(wrap(flowName(f), w)) + "\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("%d nhóm lỗi · %d lần · %s", len(f.Members), f.Total(), flowHow(f))) + "\n")
			if f.Cause != "" {
				b.WriteString(wrap("Nguyên nhân có thể: "+f.Cause, w) + "\n")
			}
			b.WriteString("\n")
			for _, s := range f.Members {
				b.WriteString(fmt.Sprintf("• %s %s\n", titleStyle.Render(s.Service), dimStyle.Render(fmt.Sprintf("%d lần", s.LastCount))))
				b.WriteString(wrap("  "+s.Signature, w) + "\n")
				if s.Caller != "" {
					b.WriteString(dimStyle.Render("  caller: "+s.Caller) + "\n")
				}
			}
			b.WriteString("\n" + dimStyle.Render("Enter mở luồng · n/f tạo một task cho cả luồng · x/z áp dụng cho mọi nhóm trong luồng") + "\n")
		} else {
			s := e.member
			if multi(e.flow) {
				b.WriteString(dimStyle.Render("thuộc luồng: "+flowName(e.flow)) + "\n")
			}
			groupDetail(&b, suggestionGroup(s), w)
			if s.Count > s.LastCount {
				b.WriteString(dimStyle.Render(fmt.Sprintf("tổng %d lần qua các lần quét, thấy lần đầu %s", s.Count, s.FirstSeen.Local().Format("02/01 15:04"))) + "\n")
			}
		}
	}
	m.logs.intro = false
	m.logs.vpContent = b.String()
	m.logs.vp.SetContent(m.logs.vpContent)
	if h := m.logs.vp.Height; sel < m.logs.vp.YOffset || sel >= m.logs.vp.YOffset+h-1 {
		m.logs.vp.SetYOffset(max(0, sel-h/3))
	}
}

// hiddenTag is the column saying how a suggestion is hidden (hidden view only).
func (m Model) hiddenTag(s *store.Suggestion) string {
	if !m.logs.suggHidden {
		return ""
	}
	tag := "bỏ qua hẳn"
	if s.Status == "snoozed" {
		tag = "ẩn tới " + s.SnoozeUntil.Local().Format("02/01")
	}
	return fmt.Sprintf("%-13s ", tag)
}

// targets are the suggestions an action on the entry applies to.
func (e suggEntry) targets() []*store.Suggestion {
	if e.member != nil {
		return []*store.Suggestion{e.member}
	}
	return e.flow.Members
}

// suggestionsDone marks suggestions once their task was submitted.
func (m Model) suggestionsDone(ss []*store.Suggestion) func(string) {
	st := m.d.Store
	ids := make([]int64, len(ss))
	for i, s := range ss {
		ids[i] = s.ID
	}
	return func(taskID string) {
		for _, id := range ids {
			_ = st.SetSuggestion(id, "task", time.Time{}, taskID)
		}
	}
}

// useSuggestionWindow makes the drill-down and the tickets cover the time the
// errors were seen.
func (m *Model) useSuggestionWindow(ss []*store.Suggestion) {
	var from, to time.Time
	for _, s := range ss {
		if !s.FirstSeen.IsZero() && (from.IsZero() || s.FirstSeen.Before(from)) {
			from = s.FirstSeen
		}
		if s.LastSeen.After(to) {
			to = s.LastSeen
		}
	}
	if from.IsZero() || from.After(to) {
		from = to.Add(-time.Hour)
	}
	m.logs.lastEarliest = from.Add(-5 * time.Minute).Format("2006-01-02T15:04:05")
	m.logs.lastLatest = to.Add(5 * time.Minute).Format("2006-01-02T15:04:05")
}

// flowContext is the main group of the entry and, for a flow, its name and
// the other groups.
func (e suggEntry) flowContext() (main splunk.ErrorGroup, title string, related []splunk.ErrorGroup) {
	if e.member != nil && !multi(e.flow) {
		return suggestionGroup(e.member), "", nil
	}
	main = suggestionGroup(e.flow.Members[0])
	if e.member != nil {
		main = suggestionGroup(e.member)
	}
	for _, s := range e.flow.Members {
		if s != e.member && (e.member != nil || s != e.flow.Members[0]) {
			related = append(related, suggestionGroup(s))
		}
	}
	return main, flowName(e.flow), related
}

// suggestKey handles the keys of the suggestion list; ok=false lets it through.
func (m Model) suggestKey(k tea.KeyMsg) (Model, tea.Cmd, bool) {
	e := m.currentEntry()
	switch k.String() {
	case "j", "down":
		m.logs.suggCursor = min(m.logs.suggCursor+1, max(0, len(m.suggEntries())-1))
	case "k", "up":
		m.logs.suggCursor = max(0, m.logs.suggCursor-1)
	case "esc":
		m.logs.suggFocus = false
	case "r":
		mm, cmd := m.scanNow()
		return mm.(Model), cmd, true
	case "v":
		m.logs.suggHidden = !m.logs.suggHidden
		m.logs.suggCursor = 0
		if err := m.loadFlows(); err != nil {
			m.msg = "đọc đề xuất: " + err.Error()
		}
	case "x", "z":
		if e == nil {
			return m, nil, true
		}
		if m.logs.suggHidden {
			m.msg = "đề xuất này đang ẩn: u bỏ ẩn, v về danh sách đề xuất"
			return m, nil, true
		}
		return m.confirmHide(e, k.String() == "x"), nil, true
	case "u":
		if e == nil || !m.logs.suggHidden {
			m.msg = "u bỏ ẩn trong danh sách đã ẩn (v)"
			return m, nil, true
		}
		ts := e.targets()
		if err := unhide(m.d.Store, ts); err != nil {
			m.msg = "bỏ ẩn: " + err.Error()
		} else {
			m.msg = fmt.Sprintf("đã bỏ ẩn %s (v về danh sách đề xuất)", entryName(*e))
		}
		m.logs.suggestions = m.suggestionCount("")
		if err := m.loadFlows(); err != nil {
			m.msg = "đọc đề xuất: " + err.Error()
		}
	case "U":
		if !m.logs.suggHidden || len(m.logs.flows) == 0 {
			m.msg = "U bỏ ẩn tất cả trong danh sách đã ẩn (v)"
			return m, nil, true
		}
		var all []*store.Suggestion
		for _, f := range m.logs.flows {
			all = append(all, f.Members...)
		}
		name, _ := m.logProject()
		st := m.d.Store
		m.conf = &confirm{text: fmt.Sprintf("Bỏ ẩn cả %d nhóm lỗi đã ẩn của %s?\nChúng quay lại danh sách đề xuất (chỉ hiện nhóm còn thấy trong khoảng quét gần nhất).", len(all), name),
			onYes: func() tea.Cmd {
				return func() tea.Msg {
					if err := unhide(st, all); err != nil {
						return suggChangedMsg("bỏ ẩn: " + err.Error())
					}
					return suggChangedMsg(fmt.Sprintf("đã bỏ ẩn %d nhóm lỗi của %s", len(all), name))
				}
			}}
	case "enter", " ":
		if e == nil {
			return m, nil, true
		}
		if e.member == nil {
			m.logs.flowOpen[e.flow.ID] = !m.logs.flowOpen[e.flow.ID]
			break
		}
		if k.String() == " " {
			return m, nil, true
		}
		m.useSuggestionWindow([]*store.Suggestion{e.member})
		spl, err := m.scoped(splunk.DrillSPL(suggestionGroup(e.member)))
		if err != nil {
			m.msg = err.Error()
			return m, nil, true
		}
		m.logs.suggFocus = false
		mm, cmd := m.runLogSearch(spl, m.logs.lastEarliest, m.logs.lastLatest, false)
		return mm.(Model), cmd, true
	case "n", "f":
		if e == nil {
			return m, nil, true
		}
		ts := e.targets()
		m.useSuggestionWindow(ts)
		g, flow, related := e.flowContext()
		var mm tea.Model
		var cmd tea.Cmd
		if k.String() == "n" {
			mm, cmd = m.investigateFlow(g, flow, related, m.suggestionsDone(ts))
		} else {
			mm, cmd = m.fixFlowTask(g, flow, related, m.suggestionsDone(ts))
		}
		return mm.(Model), cmd, true
	default:
		return m, nil, false
	}
	m.renderSuggestions()
	return m, nil, true
}

// scanDoneMsg reports a scan started with key r.
type scanDoneMsg struct {
	project string
	fresh   int
	err     error
}

// scanNow runs the project's error scan now, outside its schedule: it reads
// the time since the last good scan (at most max_window), like the daemon.
func (m Model) scanNow() (tea.Model, tea.Cmd) {
	name, _ := m.logProject()
	p := m.d.Config.Projects[name]
	switch {
	case m.d.ScanNow == nil:
		m.msg = "không quét được từ TUI; chạy: taskpilot scan run " + name
		return m, nil
	case p.LogsProduction == nil || p.LogsProduction.Scan == nil:
		m.msg = name + " chưa bật quét lỗi (logs_production.scan trong config)"
		return m, nil
	case m.logs.scanning != "":
		m.msg = "đang quét " + m.logs.scanning + ", đợi xong đã"
		return m, nil
	}
	m.logs.scanning = name
	m.msg = "đang quét lỗi production của " + name + "…"
	m.renderSuggestions()
	scan := m.d.ScanNow
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		n, err := scan(ctx, name)
		return scanDoneMsg{name, n, err}
	}
}

func (m Model) onScanDone(msg scanDoneMsg) (tea.Model, tea.Cmd) {
	m.logs.scanning = ""
	if msg.err != nil {
		m.msg = "quét " + msg.project + ": " + msg.err.Error()
	} else {
		m.msg = fmt.Sprintf("đã quét %s: %d đề xuất mới", msg.project, msg.fresh)
	}
	m.logs.suggestions = m.suggestionCount("")
	if name, _ := m.logProject(); name == msg.project {
		if err := m.loadFlows(); err == nil {
			m.renderSuggestions()
		}
	}
	return m, nil
}

// suggChangedMsg reports a change of suggestions made after a confirmation.
type suggChangedMsg string

func (m Model) onSuggChanged(msg suggChangedMsg) (tea.Model, tea.Cmd) {
	m.msg = string(msg)
	m.logs.suggestions = m.suggestionCount("")
	if err := m.loadFlows(); err != nil {
		m.msg = "đọc đề xuất: " + err.Error()
	}
	m.renderSuggestions()
	return m, nil
}

// entryName names a list entry in messages.
func entryName(e suggEntry) string {
	if e.member == nil {
		return fmt.Sprintf("luồng %s (%d nhóm)", truncate(flowName(e.flow), 50), len(e.flow.Members))
	}
	return truncate(e.member.Signature, 60)
}

// confirmHide asks before hiding an entry: x for good, z for 7 days.
func (m Model) confirmHide(e *suggEntry, forGood bool) Model {
	ts, what, st := e.targets(), entryName(*e), m.d.Store
	until := time.Now().Add(7 * 24 * time.Hour)
	text := fmt.Sprintf("Ẩn %s đến %s?\nHiện lại sau ngày đó nếu lỗi còn. Lấy lại sớm: v xem đã ẩn, u bỏ ẩn.", what, until.Local().Format("02/01"))
	if forGood {
		text = fmt.Sprintf("Bỏ qua hẳn %s?\nKhông hiện lại kể cả khi lỗi còn. Lấy lại: v xem đã ẩn, u bỏ ẩn.", what)
	}
	m.conf = &confirm{text: text, onYes: func() tea.Cmd {
		return func() tea.Msg {
			for _, s := range ts {
				var err error
				if forGood {
					err = st.SetSuggestion(s.ID, "dismissed", time.Time{}, "")
				} else {
					err = st.SetSuggestion(s.ID, "snoozed", until, "")
				}
				if err != nil {
					return suggChangedMsg("đề xuất: " + err.Error())
				}
			}
			if forGood {
				return suggChangedMsg("đã bỏ qua hẳn: " + what + " (v xem đã ẩn)")
			}
			return suggChangedMsg("ẩn đến " + until.Local().Format("02/01") + ": " + what + " (v xem đã ẩn)")
		}
	}}
	return m
}

func unhide(st *store.Store, ss []*store.Suggestion) error {
	for _, s := range ss {
		if err := st.Unhide(s.ID); err != nil {
			return err
		}
	}
	return nil
}
