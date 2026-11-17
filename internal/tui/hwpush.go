package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// hwEpicsTTL is how long the EPICs found for key h are reused.
const hwEpicsTTL = 15 * time.Minute

type (
	// hwEpicsMsg brings the current EPICs of every linked project and the
	// operator's staff id, fetched before the push form opens for taskID.
	hwEpicsMsg struct {
		taskID  string
		epics   map[string]map[hasakiwork.EpicKind]*hasakiwork.Epic // by config project
		staffID int64
		err     error
	}
	// hwPushedMsg reports the end of a push of a local task to Hasaki Work.
	hwPushedMsg struct {
		taskID string
		msg    string
	}
)

// hwProjectOptions lists the configured projects linked to a Hasaki Work project.
func (m Model) hwProjectOptions(*form) []option {
	var out []option
	for _, name := range m.d.Config.ProjectNames() {
		p := m.d.Config.Projects[name]
		if p.HasakiProjectID != 0 {
			out = append(out, option{Label: p.Name, Value: name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// hwKindOptions lists the EPIC kinds, each with the EPIC found for the
// project chosen in the form.
func (m Model) hwKindOptions(f *form) []option {
	eps := m.tasks.hwEpics[f.value("project")]
	var out []option
	for _, k := range hasakiwork.EpicKinds {
		label := k.Label() + " — chưa tìm thấy EPIC đang mở: nhập ở ô EPIC khác"
		if e := eps[k]; e != nil {
			label = fmt.Sprintf("%s — %s (%s)", k.Label(), e.Name, firstField(e.Milestone, ":"))
		}
		out = append(out, option{Label: label, Value: string(k)})
	}
	return out
}

func firstField(s, sep string) string {
	if i := strings.Index(s, sep); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

var (
	issueRe   = regexp.MustCompile(`(?i)issue[ _-]*production|production issue|hotfix|sự cố|lỗi (trên |ở )?prod`)
	improveRe = regexp.MustCompile(`(?i)improve|cải tiến|enhance|optimi[sz]|tối ưu|refactor|performance|latency`)
)

// guessEpicKind picks the EPIC kind a task most likely belongs to.
func guessEpicKind(t *store.Task) hasakiwork.EpicKind {
	s := t.Title + "\n" + t.Body
	switch {
	case issueRe.MatchString(s):
		return hasakiwork.EpicIssue
	case improveRe.MatchString(s):
		return hasakiwork.EpicImprove
	}
	return hasakiwork.EpicDev
}

// pushToHW creates a Hasaki Work task from a local task, e.g. to report work
// the agent finished: under the project's current DEV / Improve / Issue
// Production EPIC (and its sprint), assigned to the operator. The EPICs are
// looked up first (cached for a while), then the form opens.
func (m Model) pushToHW(t *store.Task) (tea.Model, tea.Cmd) {
	switch {
	case t == nil:
		return m, nil
	case m.d.HW == nil:
		m.msg = "chưa đăng nhập Hasaki Work: vào tab 4 Status và bấm l"
		return m, nil
	case t.Source == hwSource: // already on Hasaki Work: report the result on it
		return m.reportForm(t)
	case m.tasks.hwPushing[t.ID]:
		m.msg = "đang tạo task trên Hasaki Work…"
		return m, nil
	case len(m.hwProjectOptions(nil)) == 0:
		m.msg = "chưa project nào có hasaki_project_id trong config"
		return m, nil
	}
	if m.tasks.hwEpics != nil && time.Since(m.tasks.hwEpicsAt) < hwEpicsTTL {
		if m.needDraft(t) {
			return m.draftWork(t, "")
		}
		return m.hwPushForm(t)
	}
	if m.tasks.hwEpicsLoading {
		m.msg = "đang tìm EPIC trên Hasaki Work…"
		return m, nil
	}
	m.tasks.hwEpicsLoading = true
	m.msg = "đang tìm EPIC DEV / Improve / Issue Production của từng dự án…"
	hw, id := m.d.HW, t.ID
	projects := map[string]int64{}
	for _, o := range m.hwProjectOptions(nil) {
		projects[o.Value] = m.d.Config.Projects[o.Value].HasakiProjectID
	}
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		msg := hwEpicsMsg{taskID: id, epics: map[string]map[hasakiwork.EpicKind]*hasakiwork.Epic{}}
		p, err := hw.Profile(ctx)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.staffID = p.StaffID
		var mine []hasakiwork.Task
		for _, st := range []string{"_00_01", "_05", "_02"} {
			ts, err := hw.MyTasks(ctx, st)
			if err != nil {
				msg.err = err
				return msg
			}
			mine = append(mine, ts...)
		}
		for name, prid := range projects {
			eps, err := hw.CurrentEpics(ctx, prid, mine)
			if err != nil {
				msg.err = fmt.Errorf("%s: %w", name, err)
				return msg
			}
			msg.epics[name] = eps
		}
		return msg
	}
}

func (m Model) onHWEpics(msg hwEpicsMsg) (tea.Model, tea.Cmd) {
	m.tasks.hwEpicsLoading = false
	if msg.err != nil {
		m.msg = "tìm EPIC trên Hasaki Work: " + msg.err.Error()
		return m, nil
	}
	m.tasks.hwEpics, m.tasks.hwStaffID, m.tasks.hwEpicsAt = msg.epics, msg.staffID, time.Now()
	t := m.tasks.current()
	if t == nil || t.ID != msg.taskID || m.modal != nil || m.tab != tabTasks {
		m.msg = "đã tìm xong EPIC: bấm h lần nữa để tạo task"
		return m, nil
	}
	if m.needDraft(t) {
		return m.draftWork(t, "")
	}
	return m.hwPushForm(t)
}

// hwPushForm opens the form; the EPICs are in m.tasks.hwEpics.
func (m Model) hwPushForm(t *store.Task) (tea.Model, tea.Cmd) {
	// The map is shared with the closure below (which runs on a copy of m).
	if m.tasks.hwPushing == nil {
		m.tasks.hwPushing = map[string]bool{}
	}
	id, hw, cfg, st, pushing := t.ID, m.d.HW, m.d.Config, m.d.Store, m.tasks.hwPushing
	epics, staffID := m.tasks.hwEpics, m.tasks.hwStaffID
	draft, draftErr := m.workDraft(t)
	prev := ""
	if m.tasks.hwLink != nil {
		prev = m.tasks.hwLink.Code
	}
	f := newForm("Tạo task trên Hasaki Work — "+truncate(t.Title, 50), func(v map[string]string) (tea.Cmd, error) {
		p, ok := cfg.Projects[v["project"]]
		if !ok || p.HasakiProjectID == 0 {
			return nil, errors.New("chọn dự án")
		}
		nt, err := parseNewHWTask(v, p.HasakiProjectID)
		if err != nil {
			return nil, err
		}
		result, status := strings.TrimSpace(v["result"]), reportStatus(v["status"])
		actual, err := parseActualHours(v["actual"])
		if err != nil {
			return nil, err
		}
		override, err := parseEpicID(v["epic"])
		if err != nil {
			return nil, err
		}
		kind := hasakiwork.EpicKind(v["kind"])
		epic := epics[v["project"]][kind]
		if override == 0 && epic == nil {
			return nil, fmt.Errorf("chưa tìm thấy EPIC %s đang mở của %s: nhập ID EPIC ở ô EPIC khác", kind.Label(), p.Name)
		}
		if override == 0 {
			nt.ParentID, nt.MilestoneID = epic.ID, epic.MilestoneID
		}
		pushing[id] = true
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if override != 0 {
				e, err := hw.Task(ctx, override)
				if err != nil {
					return hwPushedMsg{id, fmt.Sprintf("đọc EPIC %d: %v", override, err)}
				}
				if e.ProjectID != nt.ProjectID {
					return hwPushedMsg{id, fmt.Sprintf("EPIC %d (%s) không thuộc dự án %s", override, e.Name, p.Name)}
				}
				nt.ParentID, nt.MilestoneID = e.ID, int64(e.MilestoneID)
				if nt.MilestoneID == 0 && e.Milestone != nil {
					nt.MilestoneID = e.Milestone.ID
				}
			}
			c, err := hw.CreateTask(ctx, nt)
			if err != nil {
				return hwPushedMsg{id, "tạo task trên Hasaki Work: " + err.Error()}
			}
			done := fmt.Sprintf("đã tạo %s trên Hasaki Work", c.Code)
			if prev != "" {
				done += " (thay cho " + prev + ")"
			}
			if err := st.SetHasakiLink(id, &store.HasakiLink{HWID: c.ID, Code: c.Code, CreatedAt: time.Now()}); err != nil {
				done += fmt.Sprintf(" (lưu liên kết lỗi: %v)", err)
			}
			if staffID == 0 {
				return hwPushedMsg{id, done + " nhưng chưa giao được cho bạn: không đọc được staff id"}
			}
			if err := hw.AssignStaff(ctx, c.ID, staffID); err != nil {
				return hwPushedMsg{id, done + " nhưng giao cho bạn lỗi: " + err.Error()}
			}
			done += " và giao cho bạn"
			if result != "" {
				if err := hw.Report(ctx, c.ID, staffID, result, actual, status); err != nil {
					return hwPushedMsg{id, done + ", nhưng " + err.Error()}
				}
				done += reportedSuffix(status)
			}
			return hwPushedMsg{id, done}
		}, nil
	})
	f.hint = "Ghi thật lên Hasaki Work: task nằm dưới EPIC và sprint của loại đã chọn, giao cho bạn; rồi ghi kết quả, giờ thực tế và trạng thái." +
		agentTimeHint(st, t)
	if l := m.tasks.hwLink; l != nil {
		f.hint += fmt.Sprintf("\nLưu ý: task này đã đẩy lên Hasaki Work: %s (%s). Gửi sẽ tạo thêm một task MỚI; task cũ giữ nguyên (huỷ trên web nếu không cần).",
			l.Code, l.CreatedAt.Local().Format("02/01 15:04"))
	}
	if t.State != store.StateDone {
		f.hint += "\nLưu ý: task chưa xong (" + string(t.State) + ")."
	}
	note := strings.TrimSpace(t.Body)
	if r := []rune(note); len(r) > 3000 {
		note = string(r[:3000]) + "…"
	}
	f.addSelect("project", "Dự án", m.hwProjectOptions, t.Project)
	name, result := t.Title, workResult(st, t)
	if draft != nil {
		name, result = draft.Name, withPRLinks(draft.Result, st, t)
	}
	f.hint += draftNote(draftErr, draft != nil)
	f.addText("name", "Tên task", name, true)
	f.addSelect("kind", "Loại (EPIC)", m.hwKindOptions, string(guessEpicKind(t)))
	f.addArea("note", "Mô tả (không bắt buộc)", note, false)
	f.addText("hours", "Giờ dự kiến", "8", false)
	f.addText("deadline", "Deadline (YYYY-MM-DD)", time.Now().Format("2006-01-02"), false)
	f.addArea("result", "Kết quả công việc (ghi lên task; để trống = không ghi)", result, false)
	f.addText("actual", "Giờ thực tế (giờ; để trống = không ghi)", "8", false)
	f.addSelect("status", "Trạng thái sau khi ghi kết quả", reportStatusOptions, defaultReportStatus(t))
	f.addText("epic", "EPIC khác: id hoặc link task (để trống = dùng EPIC theo Loại)", "", false)
	m.modal = f
	m.msg = "tạo task trên Hasaki Work cho " + t.ID + " (Ctrl+S gửi, Esc huỷ)"
	return m, f.start()
}

var taskIDInURL = regexp.MustCompile(`task_id=(\d+)`)

// parseEpicID reads an EPIC id typed as "123", "#123" or a task link.
func parseEpicID(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if m := taskIDInURL.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("EPIC khác phải là id số hoặc link task, không phải %q", s)
	}
	return n, nil
}

// parseNewHWTask validates the name, description, hours and deadline.
func parseNewHWTask(v map[string]string, projectID int64) (hasakiwork.NewTask, error) {
	nt := hasakiwork.NewTask{Name: strings.TrimSpace(v["name"]), Note: strings.TrimSpace(v["note"]), ProjectID: projectID}
	if nt.Name == "" {
		return nt, errors.New("nhập tên task")
	}
	if s := strings.TrimSpace(strings.ReplaceAll(v["hours"], ",", ".")); s != "" {
		h, err := strconv.ParseFloat(s, 64)
		if err != nil || h < 0 || h > 1000 {
			return nt, fmt.Errorf("giờ dự kiến không hợp lệ: %q", s)
		}
		nt.PlannedMinutes = int(math.Round(h * 60))
	}
	if s := strings.TrimSpace(v["deadline"]); s != "" {
		d, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return nt, fmt.Errorf("deadline phải có dạng YYYY-MM-DD: %q", s)
		}
		nt.DateEnd = d
	}
	return nt, nil
}
