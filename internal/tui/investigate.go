package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// investigateMsg opens the new-task form with an investigation ticket (sent
// by the symptom form, which closes first).
type investigateMsg struct {
	ticket source.Ticket
	hint   string
}

// investigationSamples is how many log lines go into a ticket.
const investigationSamples = 8

// openInvestigation opens the new-task form prefilled with the ticket, so the
// operator can read and adjust it before the agent starts.
func (m Model) openInvestigation(in splunk.Investigation, from string, after ...func(string)) (tea.Model, tea.Cmd) {
	t := source.Ticket{Project: in.Project, Repo: m.repoForService(in.Project, in.Service), Title: in.Title(), Body: in.Body()}
	hint := "Task điều tra sự cố production (" + from + "): agent tìm nguyên nhân gốc trên Splunk, code và DB QC, " +
		"báo cáo ở cổng plan; chỉ sửa code khi bạn duyệt. Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S."
	return m.newTaskForm(t, hint, after...)
}

// investigateFromGroup starts an investigation from an error group.
func (m Model) investigateFromGroup(g splunk.ErrorGroup, after ...func(string)) (tea.Model, tea.Cmd) {
	name, pl := m.logProject()
	e, l := splunk.AbsoluteWindow(m.logs.lastEarliest, m.logs.lastLatest, time.Now())
	in := splunk.Investigation{Project: name, Service: g.Service, Filter: pl.Filter, Earliest: e, Latest: l,
		Group: &g, SPL: splunk.Scope(pl.Filter, splunk.DrillSPL(g))}
	return m.openInvestigation(in, fmt.Sprintf("từ nhóm lỗi %s, %d lần", g.Service, g.Count), after...)
}

// investigateFromResults starts an investigation from the last search: its
// SPL, window and first log lines are the evidence.
func (m Model) investigateFromResults() (tea.Model, tea.Cmd) {
	name, pl := m.logProject()
	e, l := splunk.AbsoluteWindow(m.logs.lastEarliest, m.logs.lastLatest, time.Now())
	in := splunk.Investigation{Project: name, Filter: pl.Filter, Earliest: e, Latest: l, SPL: m.logs.lastSPL,
		Service: serviceOf(m.logs.lastSPL, pl.Services)}
	rows := m.logs.lastEvents[:min(investigationSamples, len(m.logs.lastEvents))]
	if splunk.IsTransforming(m.logs.lastSPL) {
		in.Samples = splunk.Table(rows, 0)
	} else {
		in.Samples = splunk.Pretty(rows, 0)
	}
	in.Symptom = fmt.Sprintf("Kết quả tìm log: %d dòng (xem truy vấn và dòng mẫu bên dưới).", len(m.logs.lastEvents))
	return m.openInvestigation(in, "từ kết quả tìm log")
}

// serviceOf returns the container a search is restricted to, if exactly one
// known service appears as kubernetes.container_name=<svc>.
func serviceOf(spl string, services []string) string {
	found := ""
	for _, s := range services {
		if strings.Contains(spl, "kubernetes.container_name="+s+" ") || strings.HasSuffix(spl, "kubernetes.container_name="+s) ||
			strings.Contains(spl, `kubernetes.container_name="`+s+`"`) {
			if found != "" {
				return ""
			}
			found = s
		}
	}
	return found
}

// investigateForm asks for a symptom reported by someone (customer, ops),
// without log evidence yet.
func (m Model) investigateForm() (tea.Model, tea.Cmd) {
	name, pl := m.logProject()
	if pl == nil {
		m.msg = "chưa có project nào có logs_production trong config"
		return m, nil
	}
	f := newForm("Điều tra sự cố production — "+name, func(v map[string]string) (tea.Cmd, error) {
		e, l := splitWindow(v["window"])
		e, l = splunk.AbsoluteWindow(e, l, time.Now())
		in := splunk.Investigation{Project: name, Symptom: v["symptom"], Service: v["service"], IDs: v["ids"],
			Filter: pl.Filter, Earliest: e, Latest: l}
		t := source.Ticket{Project: name, Repo: m.repoForService(name, in.Service), Title: in.Title(), Body: in.Body()}
		return func() tea.Msg {
			return investigateMsg{ticket: t, hint: "Task điều tra sự cố production: agent tìm nguyên nhân gốc trên Splunk, code và DB QC, " +
				"báo cáo ở cổng plan; chỉ sửa code khi bạn duyệt. Sửa tiêu đề/mô tả nếu cần rồi Ctrl+S."}
		}, nil
	})
	f.hint = "Mô tả điều đã thấy, không cần biết nguyên nhân. Khoảng thời gian được chốt thành mốc tuyệt đối để agent tìm đúng chỗ dù chạy sau."
	f.addArea("symptom", "Triệu chứng (ai báo, thấy gì, từ khi nào)", "", true)
	f.addSelect("service", "Service nghi ngờ", m.serviceOptions(true), "")
	f.addText("ids", "Mã đơn / giao dịch / request_id (nếu có)", "", false)
	f.addSelect("window", "Xảy ra trong", presetOptions, m.formWindow())
	m.modal = f
	return m, f.start()
}

// investigateFlow starts an investigation of a failing flow: its main error
// group plus the other groups of the same path.
func (m Model) investigateFlow(g splunk.ErrorGroup, flow string, related []splunk.ErrorGroup, after ...func(string)) (tea.Model, tea.Cmd) {
	if flow == "" && len(related) == 0 {
		return m.investigateFromGroup(g, after...)
	}
	name, pl := m.logProject()
	e, l := splunk.AbsoluteWindow(m.logs.lastEarliest, m.logs.lastLatest, time.Now())
	in := splunk.Investigation{Project: name, Service: g.Service, Filter: pl.Filter, Earliest: e, Latest: l,
		Group: &g, Flow: flow, Related: related, SPL: splunk.Scope(pl.Filter, splunk.DrillSPL(g)),
		Symptom: "Luồng lỗi: " + flow}
	return m.openInvestigation(in, fmt.Sprintf("từ luồng lỗi %d nhóm", len(related)+1), after...)
}
