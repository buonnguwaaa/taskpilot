package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

func (m Model) qcGroup() *argocd.ErrorGroup {
	q := m.logs.qc
	if q.view != qcViewGroups || q.groupCursor < 0 || q.groupCursor >= len(q.groups) {
		return nil
	}
	return &m.logs.qc.groups[q.groupCursor]
}

// qcGroupsKey handles the keys of the group list, the same as Production's;
// ok=false lets the key through.
func (m Model) qcGroupsKey(k tea.KeyMsg) (Model, tea.Cmd, bool) {
	q := &m.logs.qc
	switch k.String() {
	case "j", "down":
		q.groupCursor = min(q.groupCursor+1, max(0, len(q.groups)-1))
	case "k", "up":
		q.groupCursor = max(0, q.groupCursor-1)
	case "home", "g":
		q.groupCursor = 0
	case "end", "G":
		q.groupCursor = max(0, len(q.groups)-1)
	case "esc":
		if q.allGroups != nil { // undo s
			q.groups, q.allGroups, q.groupCursor = q.allGroups, nil, 0
		} else {
			q.groupsFocus = false
		}
	case "enter":
		g := m.qcGroup()
		if g == nil {
			return m, nil, true
		}
		last := q.last
		last.apps, last.text = []string{g.App}, longestPhrase(g.Signature)
		q.entries, q.view, q.groupsFocus, q.last = g.Entries, qcViewSearch, false, last
		m.refreshQC()
		m.logs.vp.GotoBottom()
		return m, nil, true
	case "s":
		g := m.qcGroup()
		if g == nil {
			return m, nil, true
		}
		if q.allGroups == nil {
			q.allGroups = q.groups
		}
		var only []argocd.ErrorGroup
		for _, x := range q.allGroups {
			if x.App == g.App {
				only = append(only, x)
			}
		}
		q.groups, q.groupCursor = only, 0
	case "n":
		mm, cmd := m.qcInvestigate()
		return mm.(Model), cmd, true
	case "f":
		g := m.qcGroup()
		if g == nil {
			return m, nil, true
		}
		mm, cmd := m.qcFixTask(*g)
		return mm.(Model), cmd, true
	default:
		return m, nil, false
	}
	m.refreshQC()
	return m, nil, true
}

// qcServiceOptions lists the project's QC apps, optionally with "all".
func (m Model) qcServiceOptions(withAll bool) func(*form) []option {
	apps := m.qcApps()
	return func(*form) []option {
		var o []option
		if withAll {
			o = append(o, option{Label: "tất cả app", Value: ""})
		}
		for _, a := range apps {
			o = append(o, option{Label: argocd.ShortApp(a.Name), Value: a.Name})
		}
		return o
	}
}

// qcRangeFor turns a window chosen in a form into an Argo CD read range.
func qcRangeFor(earliest, latest string) (since int, until time.Time, label string) {
	now := time.Now()
	if from, ok := splunk.ResolveTime(earliest, now); ok {
		since = int(now.Sub(from).Seconds())
	}
	if latest != "" {
		if to, ok := splunk.ResolveTime(latest, now); ok {
			until = to
		}
	}
	return since, until, splunk.Describe(earliest, latest)
}

// qcBuilderForm is the "search by conditions" of QC: the same fields as on
// Production, over the apps' pod logs.
func (m Model) qcBuilderForm() (tea.Model, tea.Cmd) {
	if len(m.qcApps()) == 0 {
		m.msg = "chưa có ứng dụng QC nào của project này"
		return m, nil
	}
	cur := ""
	if a := m.qcCurrent(); a != nil {
		cur = a.Name
	}
	model := m
	f := newForm("Tìm log QC — "+m.qcProject(), func(v map[string]string) (tea.Cmd, error) {
		q := qcQuery{text: strings.TrimSpace(v["text"]), requestID: strings.TrimSpace(v["request_id"]), level: v["level"]}
		if v["service"] != "" {
			q.apps = []string{v["service"]}
		}
		e, l := splitWindow(v["window"])
		q.since, q.until, q.window = qcRangeFor(e, l)
		_, cmd := model.runQC(qcViewSearch, q)
		return tea.Batch(func() tea.Msg { return qcStartedMsg{view: qcViewSearch} }, cmd), nil
	})
	f.hint = "Log QC không có trên Splunk: taskpilot đọc log pod của các app qua Argo CD rồi lọc. Kết quả xếp theo thời gian."
	f.addSelect("service", "Service (app QC)", m.qcServiceOptions(true), cur)
	f.addSelect("level", "Mức", func(*form) []option {
		return []option{{"error trở lên", splunk.LevelError}, {"warn + error", splunk.LevelWarn}, {"mọi mức", splunk.LevelAny}}
	}, splunk.LevelAny)
	f.addText("text", "Chứa chuỗi (mã giao dịch, path API, thông điệp…)", "", false)
	f.addText("request_id", "request_id", "", false)
	f.addSelect("window", "Thời gian", presetOptions, m.formWindow())
	m.modal = f
	return m, f.start()
}

// qcSymptomForm is "Điều tra sự cố" on QC: a reported symptom, no log yet.
func (m Model) qcSymptomForm() (tea.Model, tea.Cmd) {
	project := m.qcProject()
	if project == "" {
		return m, nil
	}
	f := newForm("Điều tra lỗi QC — "+project, func(v map[string]string) (tea.Cmd, error) {
		e, l := splitWindow(v["window"])
		e, l = splunk.AbsoluteWindow(e, l, time.Now())
		in := splunk.Investigation{Env: "QC", Project: project, Symptom: v["symptom"], Service: v["service"], IDs: v["ids"], Earliest: e, Latest: l}
		t := source.Ticket{Project: project, Repo: m.qcRepoFor(project, in.Service), Title: in.Title(), Body: in.Body()}
		return func() tea.Msg {
			return investigateMsg{ticket: t, hint: "Task điều tra lỗi QC: agent đọc log QC (qc_logs), trạng thái deploy (qc_apps), code và DB QC, " +
				"báo cáo ở cổng plan; chỉ sửa code khi bạn duyệt. Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S."}
		}, nil
	})
	f.hint = "Mô tả điều đã thấy trên QC, không cần biết nguyên nhân. Khoảng thời gian được chốt thành mốc tuyệt đối."
	f.addArea("symptom", "Triệu chứng (ai báo, thấy gì, từ khi nào)", "", true)
	f.addSelect("service", "Service nghi ngờ (app QC)", m.qcServiceOptions(true), "")
	f.addText("ids", "Mã đơn / giao dịch / request_id (nếu có)", "", false)
	f.addSelect("window", "Xảy ra trong", presetOptions, m.formWindow())
	m.modal = f
	return m, f.start()
}

// qcRepoFor maps an Argo CD app (payment-qc-payment-vnpay) to the project repo
// whose name ends it (payment-vnpay); the longest match wins.
func (m Model) qcRepoFor(project, app string) string {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
	best := ""
	for _, r := range m.d.Config.Projects[project].RepoNames() {
		if strings.HasSuffix(norm(app), norm(r)) && len(r) > len(best) {
			best = r
		}
	}
	return best
}

// qcWindow is the absolute window of what was read: the chosen range, or
// from the oldest line when only the latest lines were read.
func qcWindow(q qcQuery, oldest time.Time) (string, string) {
	now := time.Now()
	from, to := now.Add(-time.Hour), now
	if q.since > 0 {
		from = now.Add(-time.Duration(q.since) * time.Second)
	} else if !oldest.IsZero() {
		from = oldest
	}
	if !q.until.IsZero() {
		to = q.until
	}
	return from.Format("2006-01-02T15:04:05"), to.Format("2006-01-02T15:04:05")
}

func formatEntries(es []argocd.Entry, n int) []string {
	var out []string
	for _, e := range es[max(0, len(es)-n):] {
		out = append(out, e.Format())
	}
	return out
}

// qcInvestigate starts a QC investigation from what is on screen: the
// selected error group, or the lines shown (a search, an app's logs).
func (m Model) qcInvestigate() (tea.Model, tea.Cmd) {
	q := m.logs.qc
	project := m.qcProject()
	if project == "" {
		return m, nil
	}
	in := splunk.Investigation{Env: "QC", Project: project}
	var entries []argocd.Entry
	from := "log QC"
	switch {
	case q.view == qcViewGroups && m.qcGroup() != nil:
		g := m.qcGroup()
		in.Service = g.App
		in.Group = &splunk.ErrorGroup{Service: g.App, Signature: g.Signature, Count: g.Count, First: g.First, Last: g.Last,
			Sample: g.Sample, Caller: g.Caller, Prefix: g.Prefix, RequestID: g.RequestID, Stacktrace: g.Stacktrace}
		entries = g.Entries
		in.SPL = fmt.Sprintf("qc_logs app=%s filter=%q", g.App, longestPhrase(g.Signature))
		from = fmt.Sprintf("nhóm lỗi %s, %d lần", argocd.ShortApp(g.App), g.Count)
	case len(q.entries) > 0 && (q.view == qcViewSearch || q.logsFor != ""):
		entries = q.entries
		var errs []argocd.Entry
		for _, e := range entries {
			if e.IsError() {
				errs = append(errs, e)
			}
		}
		if len(errs) > 0 {
			entries = errs
		}
		apps := map[string]bool{}
		for _, e := range entries {
			apps[e.App] = true
		}
		if len(apps) == 1 {
			for a := range apps {
				in.Service = a
			}
		}
		filter := q.last.text
		if filter == "" {
			filter = q.last.requestID
		}
		target := "mọi app của " + project
		if len(q.last.apps) == 1 {
			target = "app=" + q.last.apps[0]
		}
		in.SPL = fmt.Sprintf("qc_logs %s filter=%q", target, filter)
		in.Symptom = fmt.Sprintf("%s: %d dòng (%d dòng lỗi) — xem dòng mẫu bên dưới.", q.last.describe(project), len(q.entries), len(errs))
		from = "các dòng log đang xem"
	default:
		m.msg = "chưa có log nào đang xem: chọn app rồi Enter, g (nhóm lỗi), / (tìm), hoặc mục Điều tra sự cố để mô tả triệu chứng"
		return m, nil
	}
	oldest := time.Time{}
	if len(entries) > 0 {
		oldest = entries[0].At
	}
	in.Earliest, in.Latest = qcWindow(q.last, oldest)
	in.Samples = formatEntries(entries, investigationSamples)
	t := source.Ticket{Project: project, Repo: m.qcRepoFor(project, in.Service), Title: in.Title(), Body: in.Body()}
	return m.newTaskForm(t, "Task điều tra lỗi QC ("+from+"): agent đọc log QC (qc_logs), trạng thái deploy (qc_apps), code và DB QC, "+
		"báo cáo ở cổng plan; chỉ sửa code khi bạn duyệt. Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S.")
}

// qcFixTask opens a task to fix a QC error group directly.
func (m Model) qcFixTask(g argocd.ErrorGroup) (tea.Model, tea.Cmd) {
	project := m.qcProject()
	var b strings.Builder
	fmt.Fprintf(&b, "Lỗi trên QC của app `%s` (dự án %s): %d lần", g.App, project, g.Count)
	if !g.First.IsZero() {
		fmt.Fprintf(&b, ", lần đầu %s, gần nhất %s", g.First.Local().Format("02/01 15:04"), g.Last.Local().Format("02/01 15:04"))
	}
	fmt.Fprintf(&b, ".\n\nThông điệp (đã chuẩn hoá): %s\nVí dụ thật: %s\n", g.Signature, g.Sample)
	for _, kv := range [][2]string{{"Caller", g.Caller}, {"Hàm (prefix)", g.Prefix}, {"request_id gần nhất", g.RequestID}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv[0], kv[1])
		}
	}
	fmt.Fprintf(&b, "\nXem lại log: qc_logs app=%s filter=%q\n", g.App, longestPhrase(g.Signature))
	b.WriteString("\nAcceptance criteria:\n- Xác định nguyên nhân gốc (log QC, code tại caller, dữ liệu DB QC nếu cần).\n" +
		"- Sửa để lỗi không còn trên QC, hoặc hạ mức log / trả lỗi rõ ràng nếu đây là lỗi dự kiến.\n- Có test tái hiện trường hợp gây lỗi.\n")
	t := source.Ticket{Project: project, Repo: m.qcRepoFor(project, g.App),
		Title: "Sửa lỗi QC " + argocd.ShortApp(g.App) + ": " + truncate(strings.TrimSpace(g.Signature), 70), Body: b.String()}
	return m.newTaskForm(t, fmt.Sprintf("Từ nhóm lỗi QC %s (%d lần). Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S.", g.App, g.Count))
}

// longestPhrase is the longest run of a signature without masked parts.
func longestPhrase(sig string) string {
	best := ""
	for _, p := range strings.FieldsFunc(sig, func(r rune) bool { return strings.ContainsRune(`<>"`, r) }) {
		p = strings.Trim(strings.TrimSpace(p), ` :,;.`)
		if p == "n" || p == "id" || p == "uuid" {
			continue
		}
		if len(p) > len(best) {
			best = p
		}
	}
	return best
}
