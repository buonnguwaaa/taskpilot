package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
)

var qcSince = []struct {
	label   string
	seconds int
}{{"mới nhất", 0}, {"15 phút", 900}, {"1 giờ", 3600}, {"4 giờ", 14400}, {"24 giờ", 86400}}

type (
	qcAppsMsg struct {
		apps []argocd.App
		err  error
	}
	qcLogsMsg struct {
		app    string
		filter string
		lines  []argocd.LogLine
		err    error
	}
	qcSyncMsg struct {
		app string
		err error
	}
)

type qcState struct {
	proj     int
	cursor   int
	all      []argocd.App
	loaded   bool
	loading  bool
	err      string
	sinceIdx int
	filter   string
	logsFor  string
	lines    []argocd.LogLine
	logsErr  string
	fetching bool
}

// qcProjects are the projects shown in the QC view (all configured ones).
func (m Model) qcProjects() []string { return m.d.Config.ProjectNames() }

func (m Model) qcApps() []argocd.App {
	ps := m.qcProjects()
	if len(ps) == 0 {
		return nil
	}
	name := ps[m.logs.qc.proj%len(ps)]
	p := m.d.Config.Projects[name]
	return argocd.Match(m.logs.qc.all, p.ArgoCD.Apps, p.ArgoCD.Projects, p.RepoNames(), p.ArgoCD.MatchesRepos(), m.d.Config.AllRepoNames())
}

func (m Model) qcCurrent() *argocd.App {
	apps := m.qcApps()
	if m.logs.qc.cursor < 0 || m.logs.qc.cursor >= len(apps) {
		return nil
	}
	return &apps[m.logs.qc.cursor]
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
		m.logs.qc.cursor = min(m.logs.qc.cursor, max(0, len(m.qcApps())-1))
	}
	m.refreshQC()
	return m, nil
}

func (m Model) loadQCLogs() (tea.Model, tea.Cmd) {
	app := m.qcCurrent()
	if app == nil || m.d.ArgoCD == nil {
		return m, nil
	}
	a, get := *app, m.d.ArgoCD
	filter := m.logs.qc.filter
	since := qcSince[m.logs.qc.sinceIdx].seconds
	tail := m.d.Config.ArgoCD.MaxLogLines
	m.logs.qc.fetching = true
	m.refreshQC()
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, err := get(ctx)
		if err != nil {
			return qcLogsMsg{app: a.Name, filter: filter, err: err}
		}
		lines, err := c.Logs(ctx, a, argocd.LogOptions{TailLines: tail, SinceSeconds: since, Filter: filter})
		return qcLogsMsg{app: a.Name, filter: filter, lines: lines, err: err}
	}
}

func (m Model) onQCLogs(msg qcLogsMsg) (tea.Model, tea.Cmd) {
	m.logs.qc.fetching = false
	m.logs.qc.logsFor = msg.app
	m.logs.qc.lines = argocd.Filter(msg.lines, msg.filter) // also filter locally (older servers ignore the param)
	m.logs.qc.logsErr = ""
	if msg.err != nil {
		m.logs.qc.logsErr = msg.err.Error()
	}
	m.refreshQC()
	m.logs.vp.GotoBottom()
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

// refreshQC renders the QC detail pane: app state, then its logs.
func (m *Model) refreshQC() {
	if m.logs.env != envQC {
		return
	}
	w := max(20, m.logs.vp.Width)
	q := &m.logs.qc
	var b strings.Builder
	app := m.qcCurrent()
	switch {
	case q.err != "":
		b.WriteString(errStyle.Render(wrap(q.err, w)))
	case q.loading:
		b.WriteString(dimStyle.Render("Đang tải ứng dụng từ Argo CD…"))
	case app == nil:
		b.WriteString(dimStyle.Render(wrap("Không có ứng dụng nào của project này trên Argo CD. Kiểm tra bằng `:argo apps` và khai báo argocd.apps / argocd.projects cho project trong config.", w)))
	default:
		fmt.Fprintf(&b, "%s  %s  %s\n", titleStyle.Render(app.Name), healthBadge(app.Health), syncBadge(app.Sync))
		rev := app.Revision
		if len(rev) > 12 {
			rev = rev[:12]
		}
		fmt.Fprintf(&b, "%s\n", dimStyle.Render(fmt.Sprintf("namespace %s · nhánh %s · revision %s", app.Namespace, app.TargetRevision, rev)))
		if app.OperationPhase != "" {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render(wrap(fmt.Sprintf("lần sync gần nhất: %s %s %s", app.OperationPhase, app.FinishedAt, firstLine(app.OperationMsg)), w)))
		}
		for _, img := range app.Images {
			fmt.Fprintf(&b, "%s\n", dimStyle.Render("image "+img))
		}
		b.WriteString("\n")
		switch {
		case q.fetching:
			b.WriteString(dimStyle.Render("Đang tải log…"))
		case q.logsFor != app.Name:
			b.WriteString(dimStyle.Render("Enter: xem log · / lọc · t khoảng thời gian · o mở Argo CD · S sync"))
		case q.logsErr != "":
			b.WriteString(errStyle.Render(wrap(q.logsErr, w)))
		default:
			head := fmt.Sprintf("%d dòng log (%s", len(q.lines), qcSince[q.sinceIdx].label)
			if q.filter != "" {
				head += fmt.Sprintf(", lọc %q", q.filter)
			}
			b.WriteString(dimStyle.Render(head+") · Enter tải lại") + "\n")
			for _, l := range q.lines {
				line := truncate(shortStamp(l.Time)+" "+strings.TrimSpace(l.Content), w)
				lower := strings.ToLower(l.Content)
				switch {
				case strings.Contains(lower, `"level":"error"`) || strings.Contains(lower, "panic") || strings.Contains(lower, " error "):
					b.WriteString(errStyle.Render(line) + "\n")
				case strings.Contains(lower, `"level":"warn"`):
					b.WriteString(warnStyle.Render(line) + "\n")
				default:
					b.WriteString(line + "\n")
				}
			}
		}
	}
	m.logs.vp.SetContent(b.String())
	m.logs.vpContent = b.String()
}

func shortStamp(ts string) string {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.Local().Format("15:04:05")
	}
	return ts
}

func healthBadge(h string) string {
	c := map[string]string{"Healthy": "10", "Progressing": "12", "Degraded": "9", "Missing": "208", "Suspended": "8"}[h]
	if c == "" {
		c = "8"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(h)
}

func syncBadge(s string) string {
	c := map[string]string{"Synced": "10", "OutOfSync": "11"}[s]
	if c == "" {
		c = "8"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(s)
}

func (m Model) qcView() string {
	h := m.innerHeight()
	lw := m.listWidth()
	var b strings.Builder
	b.WriteString(m.envLine() + "\n")
	ps := m.qcProjects()
	var tabs []string
	for i, p := range ps {
		if i == m.logs.qc.proj%max(1, len(ps)) {
			tabs = append(tabs, activeTabStyle.Render(" "+p+" "))
		} else {
			tabs = append(tabs, tabStyle.Render(" "+p+" "))
		}
	}
	b.WriteString(strings.Join(tabs, " ") + dimStyle.Render("  ←/→ · "+m.d.Config.ArgoCD.Env) + "\n")
	apps := m.qcApps()
	rows := m.listRows() - 2
	start := 0
	if m.logs.qc.cursor >= rows {
		start = m.logs.qc.cursor - rows + 1
	}
	for i := start; i < len(apps) && i-start < rows; i++ {
		a := apps[i]
		text := fmt.Sprintf("%-11s %-10s %s", a.Health, a.Sync, truncate(a.Name, lw-26))
		if i == m.logs.qc.cursor {
			b.WriteString(selStyle.Render(text) + "\n")
		} else {
			b.WriteString(fmt.Sprintf("%s %s %s", padBadge(healthBadge(a.Health), a.Health, 11), padBadge(syncBadge(a.Sync), a.Sync, 10), truncate(a.Name, lw-26)) + "\n")
		}
	}
	if len(apps) == 0 && m.logs.qc.loaded {
		b.WriteString(dimStyle.Render("(không có ứng dụng)") + "\n")
	}
	left := boxStyle.Width(lw).Height(h).Render(b.String())
	right := boxStyle.Width(m.logs.vp.Width + 2).Height(h).Render(m.logs.vp.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func padBadge(styled, plain string, w int) string {
	if n := w - len([]rune(plain)); n > 0 {
		return styled + strings.Repeat(" ", n)
	}
	return styled
}

func (m Model) qcKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	apps := m.qcApps()
	switch k.String() {
	case "left", "h", "[":
		if n := len(m.qcProjects()); n > 0 {
			m.logs.qc.proj = (m.logs.qc.proj + n - 1) % n
			m.logs.qc.cursor = 0
		}
	case "right", "l", "]":
		if n := len(m.qcProjects()); n > 0 {
			m.logs.qc.proj = (m.logs.qc.proj + 1) % n
			m.logs.qc.cursor = 0
		}
	case "j", "down":
		m.logs.qc.cursor = min(m.logs.qc.cursor+1, max(0, len(apps)-1))
	case "k", "up":
		m.logs.qc.cursor = max(0, m.logs.qc.cursor-1)
	case "pgdown", "ctrl+d", " ":
		m.logs.vp.HalfViewDown()
		return m, nil
	case "pgup", "ctrl+u":
		m.logs.vp.HalfViewUp()
		return m, nil
	case "enter":
		return m.loadQCLogs()
	case "t":
		m.logs.qc.sinceIdx = (m.logs.qc.sinceIdx + 1) % len(qcSince)
		m.msg = "log QC: " + qcSince[m.logs.qc.sinceIdx].label + " (Enter để tải)"
	case "/":
		f := newForm("Lọc log QC", func(v map[string]string) (tea.Cmd, error) { return nil, nil })
		f.addText("filter", "Chỉ hiện dòng chứa (để trống: bỏ lọc)", m.logs.qc.filter, false)
		model := m
		f.onSubmit = func(v map[string]string) (tea.Cmd, error) {
			model.logs.qc.filter = v["filter"]
			_, cmd := model.loadQCLogs()
			return tea.Batch(cmd, func() tea.Msg { return qcFilterMsg(v["filter"]) }), nil
		}
		m.modal = f
		return m, f.start()
	case "r":
		return m.loadQCApps()
	case "o":
		if a := m.qcCurrent(); a != nil {
			if err := m.d.OpenURL(argocd.AppURL(m.d.Config.ArgoCD.URL, a.Name)); err != nil {
				m.msg = "mở trình duyệt: " + err.Error()
			} else {
				m.msg = "đã mở " + a.Name + " trên Argo CD"
			}
		}
	case "S":
		a := m.qcCurrent()
		if a == nil || m.d.ArgoCD == nil {
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

// qcFilterMsg carries a filter chosen in the form back into the model.
type qcFilterMsg string
