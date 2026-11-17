package tui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Reporting on Hasaki Work: the work result ("Kết quả công việc") and the
// status of the operator's assignment (done / waiting for approval).

var reportStatusOptions = func(*form) []option {
	return []option{
		{"Hoàn thành", strconv.Itoa(hasakiwork.StatusFinished)},
		{"Chờ duyệt", strconv.Itoa(hasakiwork.StatusWaitingApproval)},
		{"Không đổi (chỉ ghi kết quả)", "0"},
	}
}

func reportStatus(v string) int {
	n, _ := strconv.Atoi(v)
	return n
}

// defaultReportStatus: done tasks are reported done, others keep their status.
func defaultReportStatus(t *store.Task) string {
	if t.State == store.StateDone {
		return strconv.Itoa(hasakiwork.StatusFinished)
	}
	return "0"
}

// parseActualHours reads "Giờ thực tế" (hours, "2,5" allowed) as minutes;
// empty is 0 (leave it unchanged).
func parseActualHours(v string) (int, error) {
	v = strings.TrimSpace(strings.ReplaceAll(v, ",", "."))
	if v == "" {
		return 0, nil
	}
	h, err := strconv.ParseFloat(v, 64)
	if err != nil || h <= 0 || h > 200 {
		return 0, fmt.Errorf("giờ thực tế không hợp lệ: %q (nhập số giờ, vd 2 hoặc 1,5)", v)
	}
	return int(h*60 + 0.5), nil
}

// hoursText formats minutes as hours for a form field ("2", "1,5").
func hoursText(minutes float64) string {
	if minutes <= 0 {
		return ""
	}
	return strings.Replace(strconv.FormatFloat(minutes/60, 'f', -1, 64), ".", ",", 1)
}

// agentTimeHint says how long the agent worked, as a reference for the
// actual time.
func agentTimeHint(st *store.Store, t *store.Task) string {
	d := st.RunTime(t.ID)
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("\nAgent đã chạy %s cho task này (tham khảo khi nhập giờ thực tế).", d.Round(time.Minute))
}

func reportedSuffix(status int) string {
	switch status {
	case hasakiwork.StatusFinished:
		return ", đã ghi kết quả và đánh hoàn thành"
	case hasakiwork.StatusWaitingApproval:
		return ", đã ghi kết quả và chuyển chờ duyệt"
	}
	return ", đã ghi kết quả"
}

// workResult drafts the work result from what taskpilot knows: the
// completion summary (or the agent's last message) and the pull requests.
func workResult(st *store.Store, t *store.Task) string {
	var b strings.Builder
	if c, _ := st.Completion(t.ID); c != nil && strings.TrimSpace(c.Summary) != "" {
		b.WriteString(strings.TrimSpace(c.Summary))
	} else if r := strings.TrimSpace(t.LastResult); r != "" && t.State == store.StateDone {
		b.WriteString(r)
	}
	return strings.TrimSpace(withPRLinks(b.String(), st, t))
}

var hwIDInBody = regexp.MustCompile(`\(id (\d+)\)`)

// sourceHWTask is the Hasaki Work task a dispatched task came from: its code
// and numeric id (written in the ticket body when it was dispatched).
func (m Model) sourceHWTask(t *store.Task) (code string, id int64) {
	code = t.TicketID
	if mm := hwIDInBody.FindStringSubmatch(t.Body); mm != nil {
		id, _ = strconv.ParseInt(mm[1], 10, 64)
	}
	if id == 0 {
		for _, x := range m.hw.list {
			if x.Code == code {
				id = x.ID
			}
		}
	}
	return code, id
}

// suggestedHours is the agent's run time rounded up to half an hour, as a
// starting point for the actual time ("" when the agent never ran).
func suggestedHours(st *store.Store, t *store.Task) string {
	d := st.RunTime(t.ID)
	if d <= 0 {
		return ""
	}
	halves := int((d + 30*time.Minute - 1) / (30 * time.Minute))
	return hoursText(float64(halves * 30))
}

// reportForm reports work done on a task that came from Hasaki Work: it
// creates a SUBTASK under it (same project and sprint), assigns it to the
// operator, writes the work result and the actual time on the subtask and
// sets the subtask's status. The parent task is left as it is. This WRITES
// to Hasaki Work.
func (m Model) reportForm(t *store.Task) (tea.Model, tea.Cmd) {
	if m.d.HW == nil {
		m.msg = "chưa đăng nhập Hasaki Work: vào tab 4 Status và bấm l"
		return m, nil
	}
	code, hwID := m.sourceHWTask(t)
	if hwID == 0 {
		m.msg = "không tìm thấy id của " + code + " trên Hasaki Work: mở tab 2 để tải danh sách rồi thử lại"
		return m, nil
	}
	if m.tasks.hwPushing == nil {
		m.tasks.hwPushing = map[string]bool{}
	}
	if m.tasks.hwPushing[t.ID] {
		m.msg = "đang tạo subtask trên Hasaki Work…"
		return m, nil
	}
	if m.needDraft(t) {
		parent := code
		for _, x := range m.hw.list {
			if x.ID == hwID {
				parent = code + " — " + x.Name
			}
		}
		return m.draftWork(t, parent)
	}
	draft, draftErr := m.workDraft(t)
	hw, st, id, pushing := m.d.HW, m.d.Store, t.ID, m.tasks.hwPushing
	prev := ""
	if m.tasks.hwLink != nil {
		prev = m.tasks.hwLink.Code
	}
	f := newForm("Subtask hoàn thành dưới "+code+" — "+truncate(t.Title, 50), func(v map[string]string) (tea.Cmd, error) {
		name, result := strings.TrimSpace(v["name"]), strings.TrimSpace(v["result"])
		if name == "" {
			return nil, fmt.Errorf("nhập tên subtask")
		}
		if result == "" {
			return nil, fmt.Errorf("nhập kết quả công việc (Hasaki Work không cho hoàn thành khi chưa có kết quả)")
		}
		actual, err := parseActualHours(v["actual"])
		if err != nil {
			return nil, err
		}
		status := reportStatus(v["status"])
		pushing[id] = true
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			done := func(msg string) tea.Msg { return hwPushedMsg{id, msg} }
			parent, err := hw.Task(ctx, hwID)
			if err != nil {
				return done("đọc " + code + ": " + err.Error())
			}
			p, err := hw.Profile(ctx)
			if err != nil || p.StaffID == 0 {
				return done(fmt.Sprintf("không đọc được staff id của bạn (%v)", err))
			}
			nt := hasakiwork.NewTask{Name: name, ProjectID: parent.ProjectID, ParentID: parent.ID,
				MilestoneID: int64(parent.MilestoneID), PlannedMinutes: actual}
			nt.DateEnd, _ = time.ParseInLocation("2006-01-02", subtaskDeadline(*parent, time.Now()), time.Local)
			c, err := hw.CreateTask(ctx, nt)
			if err != nil {
				return done("tạo subtask dưới " + code + ": " + err.Error())
			}
			msg := fmt.Sprintf("đã tạo subtask %s dưới %s", c.Code, code)
			if prev != "" {
				msg += " (lần trước: " + prev + ")"
			}
			if err := st.SetHasakiLink(id, &store.HasakiLink{HWID: c.ID, Code: c.Code, CreatedAt: time.Now()}); err != nil {
				msg += fmt.Sprintf(" (lưu liên kết lỗi: %v)", err)
			}
			if err := hw.AssignStaff(ctx, c.ID, p.StaffID); err != nil {
				return done(msg + " nhưng giao cho bạn lỗi: " + err.Error())
			}
			if err := hw.Report(ctx, c.ID, p.StaffID, result, actual, status); err != nil {
				return done(msg + ", giao cho bạn, nhưng " + err.Error())
			}
			return done(msg + ", giao cho bạn" + reportedSuffix(status) + "; " + code + " giữ nguyên")
		}, nil
	})
	f.hint = "Ghi thật lên Hasaki Work: tạo subtask dưới " + code + " (cùng dự án và sprint), giao cho bạn, ghi kết quả + giờ thực tế " +
		"rồi đánh trạng thái cho SUBTASK. Task " + code + " không bị đổi." + agentTimeHint(m.d.Store, t) + draftNote(draftErr, draft != nil)
	if l := m.tasks.hwLink; l != nil {
		f.hint += fmt.Sprintf("\nLưu ý: đã tạo %s cho task này lúc %s: gửi nữa sẽ tạo thêm một subtask.", l.Code, l.CreatedAt.Local().Format("02/01 15:04"))
	}
	if t.State != store.StateDone {
		f.hint += "\nLưu ý: task trong taskpilot chưa xong (" + string(t.State) + ")."
	}
	name, result := t.Title, workResult(st, t)
	if draft != nil {
		name, result = draft.Name, withPRLinks(draft.Result, st, t)
	}
	f.addText("name", "Tên subtask", name, true)
	f.addArea("result", "Kết quả công việc", result, true)
	f.addText("actual", "Giờ thực tế (giờ, vd 2 hoặc 1,5; cũng là giờ dự kiến của subtask)", suggestedHours(st, t), false)
	f.addSelect("status", "Trạng thái của subtask", reportStatusOptions, strconv.Itoa(hasakiwork.StatusFinished))
	m.modal = f
	m.msg = "tạo subtask hoàn thành dưới " + code + " (Ctrl+S gửi)"
	return m, f.start()
}

// withPRLinks makes sure the result lists the task's pull request links
// (the drafter is told to, but a link must not depend on it).
func withPRLinks(result string, st *store.Store, t *store.Task) string {
	prs, _ := st.PullRequests(t.ID)
	var missing []string
	for _, pr := range sortedPRs(prs) {
		if !pr.Pending && pr.URL != "" && !strings.Contains(result, pr.URL) {
			missing = append(missing, fmt.Sprintf("- %s → %s: %s", pr.Repo, pr.Destination, pr.URL))
		}
	}
	if len(missing) == 0 {
		return result
	}
	return strings.TrimSpace(result) + "\n\nPull request:\n" + strings.Join(missing, "\n")
}
