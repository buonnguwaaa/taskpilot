package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// QC logs are not in Splunk: they are read from Argo CD (pod logs of each
// application) and searched or grouped locally. The layout and keys are the
// Production ones (logs_common.go).

type qcView int

const (
	qcViewApp    qcView = iota // the selected app: its state and its pod logs
	qcViewGroups               // errors of the apps, grouped
	qcViewSearch               // lines of the apps matching a search
)

// qcActions are the shared entries above the applications.
var qcActions = []struct{ name, note string }{
	{itemGroups, "Gom lỗi giống nhau (số, mã, UUID được che) của mọi app QC; chọn một nhóm để xem log, điều tra hoặc tạo task sửa."},
	{itemInvestigate, "QC báo lỗi? Mô tả triệu chứng, mã đơn/giao dịch, khoảng thời gian: tạo task để agent điều tra trên log QC, code và DB QC, báo cáo trước khi sửa."},
	{itemBuilder, "Chọn app, mức, chuỗi cần tìm, request_id: tìm trên log của các app QC, xếp theo thời gian (lần theo một request qua các service)."},
}

// qcQuery is one read of the Argo CD logs.
type qcQuery struct {
	apps      []string // application names; empty = every app of the project
	text      string   // substring (sent to Argo CD as its filter)
	requestID string
	level     string // splunk.LevelError / LevelWarn / LevelAny
	since     int    // seconds back; 0 = the latest lines
	until     time.Time
	window    string // label of the window
}

func (q qcQuery) describe(project string) string {
	var parts []string
	if q.text != "" {
		parts = append(parts, fmt.Sprintf("%q", q.text))
	}
	if q.requestID != "" {
		parts = append(parts, "request_id="+q.requestID)
	}
	switch q.level {
	case splunk.LevelError:
		parts = append(parts, "lỗi")
	case splunk.LevelWarn:
		parts = append(parts, "warn+lỗi")
	}
	where := "mọi app QC của " + project
	if len(q.apps) == 1 {
		where = q.apps[0]
	} else if len(q.apps) > 1 {
		where = strings.Join(q.apps, ", ")
	}
	if len(parts) == 0 {
		return "Log " + where
	}
	return "Tìm " + strings.Join(parts, " · ") + " trên " + where
}

type (
	qcAppsMsg struct {
		apps []argocd.App
		err  error
	}
	// qcResultMsg brings the logs of one read.
	qcResultMsg struct {
		view    qcView
		query   qcQuery
		entries []argocd.Entry
		errs    []string // apps whose logs could not be read
		took    time.Duration
	}
	// qcStartedMsg marks a read as started by a form (which ran on a copy).
	qcStartedMsg struct{ view qcView }
	qcSyncMsg    struct {
		app string
		err error
	}
)

type qcState struct {
	proj    int
	row     int // the cursor over the actions then the apps
	all     []argocd.App
	loaded  bool
	loading bool
	err     string

	fetching bool
	view     qcView
	last     qcQuery // the last read (r runs it again)
	entries  []argocd.Entry
	logsFor  string // app of entries when view == qcViewApp
	errs     []string
	took     time.Duration

	groups      []argocd.ErrorGroup
	allGroups   []argocd.ErrorGroup // before s narrowed them to one app
	groupCursor int
	groupsFocus bool
}

// qcProjects are the projects shown in the QC view (all configured ones).
func (m Model) qcProjects() []string { return m.d.Config.ProjectNames() }

func (m Model) qcProject() string {
	ps := m.qcProjects()
	if len(ps) == 0 {
		return ""
	}
	return ps[m.logs.qc.proj%len(ps)]
}

func (m Model) qcApps() []argocd.App {
	name := m.qcProject()
	if name == "" {
		return nil
	}
	p := m.d.Config.Projects[name]
	return argocd.Match(m.logs.qc.all, p.ArgoCD.Apps, p.ArgoCD.Projects, p.RepoNames(), p.ArgoCD.MatchesRepos(), m.d.Config.AllRepoNames())
}

// qcCurrent is the app under the cursor (nil on an action row).
func (m Model) qcCurrent() *argocd.App {
	apps := m.qcApps()
	i := m.logs.qc.row - len(qcActions)
	if i < 0 || i >= len(apps) {
		return nil
	}
	return &apps[i]
}

// qcRows lays out the QC list for logsLeft.
func (m Model) qcRows() []logRow {
	rows := []logRow{{plain: "── Công cụ", item: -1}}
	for i, a := range qcActions {
		rows = append(rows, logRow{plain: a.name, note: a.note, item: i})
	}
	apps := m.qcApps()
	title := "── Ứng dụng QC (" + m.d.Config.ArgoCD.Env + ") · Enter log pod · S sync"
	rows = append(rows, logRow{plain: "", item: -1}, logRow{plain: title, item: -1})
	lw := m.listWidth()
	for i, a := range apps {
		hp, sp := healthIcon(a.Health)+" "+a.Health, syncIcon(a.Sync)+" "+a.Sync
		plain := fmt.Sprintf("%-13s %-12s %s", hp, sp, truncate(a.Name, lw-32))
		styled := fmt.Sprintf("%s %s %s", padBadge(healthBadge(a.Health), hp, 13), padBadge(syncBadge(a.Sync), sp, 12), truncate(a.Name, lw-32))
		rows = append(rows, logRow{plain: plain, label: styled, item: len(qcActions) + i,
			note: fmt.Sprintf("namespace %s · nhánh %s · revision %s", a.Namespace, a.TargetRevision, shortRev(a.Revision))})
	}
	if len(apps) == 0 && m.logs.qc.loaded {
		rows = append(rows, logRow{plain: "(không có ứng dụng)", item: -1})
	}
	return rows
}

func (m Model) qcView() string {
	ps := m.qcProjects()
	return m.logsLeft(ps, m.logs.qc.proj%max(1, len(ps)), m.qcRows(), m.logs.qc.row, m.logs.qc.groupsFocus, "")
}

func (m Model) ensureQCApps() (tea.Model, tea.Cmd) {
	if m.logs.qc.loaded || m.logs.qc.loading {
		m.refreshQC()
		return m, nil
	}
	return m.loadQCApps()
}

func (m Model) loadQCApps() (tea.Model, tea.Cmd) {
	if m.d.Config.ArgoCD.URL == "" {
		m.logs.qc.err = "Chưa cấu hình argocd.url"
		m.refreshQC()
		return m, nil
	}
	if m.d.ArgoCD == nil {
		m.logs.qc.err = "Chưa đăng nhập Argo CD: tab 4 Status, phím a"
		m.refreshQC()
		return m, nil
	}
	m.logs.qc.loading, m.logs.qc.err = true, ""
	m.refreshQC()
	get := m.d.ArgoCD
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := get(ctx)
		if err != nil {
			return qcAppsMsg{err: err}
		}
		apps, err := c.Apps(ctx)
		return qcAppsMsg{apps: apps, err: err}
	}
}

func (m Model) onQCApps(msg qcAppsMsg) (tea.Model, tea.Cmd) {
	m.logs.qc.loading, m.logs.qc.loaded = false, true
	if msg.err != nil {
		m.logs.qc.err = msg.err.Error()
		if errors.Is(msg.err, argocd.ErrUnauthorized) || errors.Is(msg.err, argocd.ErrNoCredentials) {
			m.logs.qc.err += "\n→ tab 4 Status, phím a để đăng nhập Argo CD"
		}
	} else {
		m.logs.qc.all = msg.apps
		m.logs.qc.row = min(m.logs.qc.row, len(qcActions)+max(0, len(m.qcApps())-1))
	}
	m.refreshQC()
	return m, nil
}

// qcPresetQuery is a query over the window chosen with t/T.
func (m Model) qcPresetQuery() qcQuery {
	since, until := m.qcRange()
	return qcQuery{since: since, until: until, window: m.presetLabel()}
}

// runQC reads the logs of the query's apps from Argo CD (4 at a time) and
// filters them locally (level, request_id, window end).
func (m Model) runQC(view qcView, q qcQuery) (tea.Model, tea.Cmd) {
	apps := m.qcApps()
	if len(q.apps) > 0 {
		want := map[string]bool{}
		for _, a := range q.apps {
			want[a] = true
		}
		var sel []argocd.App
		for _, a := range apps {
			if want[a.Name] {
				sel = append(sel, a)
			}
		}
		apps = sel
	}
	if len(apps) == 0 || m.d.ArgoCD == nil {
		m.msg = "không có ứng dụng QC nào để đọc log"
		return m, nil
	}
	get := m.d.ArgoCD
	tail := m.d.Config.ArgoCD.MaxLogLines
	if view != qcViewApp {
		tail = max(tail, qcMinTail)
	}
	m.logs.qc.view, m.logs.qc.fetching = view, true
	m.refreshQC()
	return m, func() tea.Msg {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		msg := qcResultMsg{view: view, query: q}
		c, err := get(ctx)
		if err != nil {
			msg.errs = []string{err.Error()}
			return msg
		}
		filter := q.text
		if filter == "" {
			filter = q.requestID
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, a := range apps {
			wg.Add(1)
			go func(a argocd.App) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				lines, err := c.Logs(ctx, a, argocd.LogOptions{TailLines: tail, SinceSeconds: q.since, Filter: filter})
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					msg.errs = append(msg.errs, a.Name+": "+err.Error())
					return
				}
				for _, l := range argocd.Filter(lines, filter) {
					e := argocd.ParseEntry(a.Name, l)
					if q.keep(e) {
						msg.entries = append(msg.entries, e)
					}
				}
			}(a)
		}
		wg.Wait()
		sort.SliceStable(msg.entries, func(i, j int) bool { return msg.entries[i].At.Before(msg.entries[j].At) })
		msg.took = time.Since(start)
		return msg
	}
}

// qcMinTail is the least number of lines read per app for grouping/search.
const qcMinTail = 500

// keep applies the filters Argo CD cannot: level, request_id, window end.
func (q qcQuery) keep(e argocd.Entry) bool {
	if !q.until.IsZero() && !e.At.IsZero() && !e.At.Before(q.until) {
		return false
	}
	if q.requestID != "" && q.text != "" && e.RequestID != q.requestID && !strings.Contains(e.Raw, q.requestID) {
		return false
	}
	switch q.level {
	case splunk.LevelError:
		return e.IsError()
	case splunk.LevelWarn:
		return e.IsError() || e.Level == "warn"
	}
	return true
}

func (m Model) onQCResult(msg qcResultMsg) (tea.Model, tea.Cmd) {
	q := &m.logs.qc
	q.fetching = false
	q.view, q.last, q.errs, q.took = msg.view, msg.query, msg.errs, msg.took
	switch msg.view {
	case qcViewGroups:
		q.groups, q.allGroups, q.groupCursor = argocd.GroupErrors(msg.entries), nil, 0
		q.groupsFocus = len(q.groups) > 0
	default:
		q.entries, q.groupsFocus = msg.entries, false
		q.logsFor = ""
		if msg.view == qcViewApp && len(msg.query.apps) == 1 {
			q.logsFor = msg.query.apps[0]
		}
	}
	m.refreshQC()
	if msg.view == qcViewGroups {
		m.logs.vp.GotoTop()
	} else {
		m.logs.vp.GotoBottom()
	}
	return m, nil
}

func (m Model) onQCSync(msg qcSyncMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.msg = "sync " + msg.app + ": " + msg.err.Error()
		return m, nil
	}
	m.msg = "đã yêu cầu sync " + msg.app + " — đang tải lại trạng thái"
	return m.loadQCApps()
}

// qcGroupsAsSplunk adapts the QC groups to the shared group list.
func qcGroupsAsSplunk(gs []argocd.ErrorGroup) []splunk.ErrorGroup {
	out := make([]splunk.ErrorGroup, len(gs))
	for i, g := range gs {
		out[i] = splunk.ErrorGroup{Service: argocd.ShortApp(g.App), Signature: g.Signature, Count: g.Count, First: g.First, Last: g.Last,
			Sample: g.Sample, Caller: g.Caller, Prefix: g.Prefix, RequestID: g.RequestID, Stacktrace: g.Stacktrace}
	}
	return out
}

// refreshQC renders the right pane of QC: the app's state and logs, the
// search results, or the error groups.
func (m *Model) refreshQC() {
	if m.logs.env != envQC {
		return
	}
	w := max(20, m.logs.vp.Width)
	q := &m.logs.qc
	var b strings.Builder
	sel := -1
	switch {
	case q.err != "":
		b.WriteString(quote(errStyle.Render(wrap(q.err, w-2)), "9"))
	case q.loading:
		b.WriteString(dimStyle.Render("◌ Đang tải ứng dụng từ Argo CD…"))
	case q.fetching:
		b.WriteString(dimStyle.Render("◌ Đang đọc log từ Argo CD…"))
	case q.view == qcViewGroups:
		m.renderQCErrs(&b, w)
		title := fmt.Sprintf("Nhóm lỗi QC — %s · %s", m.qcProject(), q.last.window)
		sel = renderGroupList(&b, title, qcGroupsAsSplunk(q.groups), q.groupCursor, q.groupsFocus, w, groupKeys)
	case q.view == qcViewSearch:
		m.renderQCErrs(&b, w)
		m.renderQCEntries(&b, q.last.describe(m.qcProject()), w)
	default:
		app := m.qcCurrent()
		if app == nil {
			if q.logsFor != "" {
				m.renderQCEntries(&b, q.last.describe(m.qcProject()), w)
			} else {
				b.WriteString(logsIntro("Chọn một app rồi Enter để xem log pod, hoặc trên mọi app QC:", [][2]string{
					{"g", "nhóm lỗi"}, {"/", "tìm theo điều kiện"}, {"n", "điều tra"}, {"t", "đổi khoảng thời gian"}}, w))
			}
			break
		}
		fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Render(app.Name))
		fmt.Fprintf(&b, "%s%s%s\n\n", healthBadge(app.Health), dimStyle.Render("  ·  "), syncBadge(app.Sync))
		fmt.Fprintf(&b, "%s\n", kv("namespace", app.Namespace))
		fmt.Fprintf(&b, "%s\n", kv("nhánh", "⎇ "+app.TargetRevision))
		fmt.Fprintf(&b, "%s\n", kv("revision", shortRev(app.Revision)))
		if app.OperationPhase != "" {
			phase := app.OperationPhase
			c := map[string]string{"Succeeded": "10", "Failed": "9", "Error": "9", "Running": "12"}[phase]
			if c == "" {
				c = "7"
			}
			line := lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(phase) + dimStyle.Render(" "+app.FinishedAt)
			if msg := firstLine(app.OperationMsg); msg != "" {
				line += "\n" + kvIndent + dimStyle.Render(wrap(msg, w-len(kvIndent)))
			}
			fmt.Fprintf(&b, "%s\n", kv("sync", strings.ReplaceAll(line, "\n", "\n"+kvIndent)))
		}
		for i, img := range app.Images {
			fmt.Fprintf(&b, "%s\n", kv(map[bool]string{true: "image", false: ""}[i == 0], dimStyle.Render(truncate(img, w-len(kvIndent)))))
		}
		b.WriteString("\n")
		if q.logsFor != app.Name {
			b.WriteString(quote(keyCap("Enter")+" xem log pod   "+keyCap("S")+" sync   "+keyCap("o")+" mở Argo CD", "12"))
			break
		}
		m.renderQCErrs(&b, w)
		m.renderQCEntries(&b, "Log pod "+app.Name, w)
	}
	m.logs.vp.SetContent(b.String())
	m.logs.vpContent = b.String()
	if sel >= 0 {
		if h := m.logs.vp.Height; sel < m.logs.vp.YOffset || sel >= m.logs.vp.YOffset+h-1 {
			m.logs.vp.SetYOffset(max(0, sel-h/3))
		}
	}
}

func (m *Model) renderQCErrs(b *strings.Builder, w int) {
	for _, e := range m.logs.qc.errs {
		b.WriteString(quote(warnStyle.Render(wrap("không đọc được log "+e, w-2)), "208") + "\n")
	}
}

// renderQCEntries draws log lines like the Production results.
func (m *Model) renderQCEntries(b *strings.Builder, title string, w int) {
	q := &m.logs.qc
	keys := "n điều tra · o Argo CD · c copy · r chạy lại"
	if len(q.groups) > 0 {
		keys += " · g về nhóm lỗi"
	}
	renderResultHeader(b, title, q.last.window, len(q.entries), q.took, false, keys, w)
	for _, e := range q.entries {
		b.WriteString(colorLogLine(truncate(e.Format(), w)) + "\n")
	}
}

func healthBadge(h string) string {
	c := map[string]string{"Healthy": "10", "Progressing": "12", "Degraded": "9", "Missing": "208", "Suspended": "8"}[h]
	if c == "" {
		c = "8"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(healthIcon(h) + " " + h)
}

func healthIcon(h string) string {
	if i := map[string]string{"Healthy": "♥", "Progressing": "◌", "Degraded": "✗", "Missing": "?", "Suspended": "‖"}[h]; i != "" {
		return i
	}
	return "·"
}

func syncIcon(s string) string {
	if i := map[string]string{"Synced": "✓", "OutOfSync": "↻"}[s]; i != "" {
		return i
	}
	return "·"
}

func syncBadge(s string) string {
	c := map[string]string{"Synced": "10", "OutOfSync": "11"}[s]
	if c == "" {
		c = "8"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(syncIcon(s) + " " + s)
}

func padBadge(styled, plain string, w int) string {
	if n := w - len([]rune(plain)); n > 0 {
		return styled + strings.Repeat(" ", n)
	}
	return styled
}

func shortRev(r string) string {
	if len(r) > 12 {
		return r[:12]
	}
	return r
}

// qcRunRow runs the entry under the cursor: an action, or an app's logs.
func (m Model) qcRunRow() (tea.Model, tea.Cmd) {
	switch r := m.logs.qc.row; {
	case r < len(qcActions):
		switch qcActions[r].name {
		case itemGroups:
			return m.runQC(qcViewGroups, m.qcPresetQuery())
		case itemInvestigate:
			return m.qcSymptomForm()
		case itemBuilder:
			return m.qcBuilderForm()
		}
	default:
		if a := m.qcCurrent(); a != nil {
			q := m.qcPresetQuery()
			q.apps = []string{a.Name}
			return m.runQC(qcViewApp, q)
		}
	}
	return m, nil
}

func (m Model) qcKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	q := &m.logs.qc
	if q.groupsFocus {
		if mm, cmd, ok := m.qcGroupsKey(k); ok {
			return mm, cmd
		}
	}
	rows := len(qcActions) + len(m.qcApps())
	switch k.String() {
	case "left", "h", "[":
		if n := len(m.qcProjects()); n > 0 {
			q.proj = (q.proj + n - 1) % n
			q.row, q.view, q.groupsFocus, q.logsFor = 0, qcViewApp, false, ""
		}
	case "right", "l", "]":
		if n := len(m.qcProjects()); n > 0 {
			q.proj = (q.proj + 1) % n
			q.row, q.view, q.groupsFocus, q.logsFor = 0, qcViewApp, false, ""
		}
	case "j", "down":
		q.row = min(q.row+1, max(0, rows-1))
		if m.qcCurrent() != nil {
			q.view = qcViewApp
		}
	case "k", "up":
		q.row = max(0, q.row-1)
		if m.qcCurrent() != nil {
			q.view = qcViewApp
		}
	case "pgdown", "ctrl+d", " ":
		m.logs.vp.HalfViewDown()
		return m, nil
	case "pgup", "ctrl+u":
		m.logs.vp.HalfViewUp()
		return m, nil
	case "enter":
		return m.qcRunRow()
	case "g":
		if len(q.groups) > 0 && q.view != qcViewGroups { // back to the last groups
			q.view, q.groupsFocus = qcViewGroups, true
			break
		}
		return m.runQC(qcViewGroups, m.qcPresetQuery())
	case "/", "F":
		return m.qcBuilderForm()
	case "n":
		return m.qcInvestigate()
	case "a":
		m.msg = "hỏi bằng lời (Claude viết SPL) chỉ có trên Production: log QC không có trên Splunk"
	case "r":
		if q.last.window == "" {
			return m.loadQCApps()
		}
		last := q.last
		if m.logs.preset > 0 { // a new window applies to the rerun
			p := m.qcPresetQuery()
			last.since, last.until, last.window = p.since, p.until, p.window
		}
		mm, cmd := m.runQC(q.view, last)
		_, apps := mm.(Model).loadQCApps()
		return mm, tea.Batch(cmd, apps)
	case "o":
		name := ""
		if a := m.qcCurrent(); a != nil {
			name = a.Name
		} else if q.logsFor != "" {
			name = q.logsFor
		}
		if name == "" {
			m.msg = "chọn một app để mở trên Argo CD"
			break
		}
		if err := m.d.OpenURL(argocd.AppURL(m.d.Config.ArgoCD.URL, name)); err != nil {
			m.msg = "mở trình duyệt: " + err.Error()
		} else {
			m.msg = "đã mở " + name + " trên Argo CD"
		}
	case "c":
		var lines []string
		for _, e := range q.entries {
			lines = append(lines, e.Format())
		}
		if len(lines) == 0 {
			m.msg = "chưa có dòng log nào để copy"
			break
		}
		if err := m.d.Copy(strings.Join(lines, "\n")); err != nil {
			m.msg = "copy: " + err.Error()
		} else {
			m.msg = fmt.Sprintf("đã copy %d dòng log", len(lines))
		}
	case "S":
		a := m.qcCurrent()
		if a == nil || m.d.ArgoCD == nil {
			m.msg = "chọn một app để sync"
			break
		}
		name, get := a.Name, m.d.ArgoCD
		m.conf = &confirm{
			text: fmt.Sprintf("Sync %s trên Argo CD %s về %s?\n(hiện: %s, %s)", name, m.d.Config.ArgoCD.Env, a.TargetRevision, a.Sync, a.Health),
			onYes: func() tea.Cmd {
				return func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					c, err := get(ctx)
					if err == nil {
						err = c.Sync(ctx, name)
					}
					return qcSyncMsg{app: name, err: err}
				}
			},
		}
		return m, nil
	}
	m.refreshQC()
	return m, nil
}
