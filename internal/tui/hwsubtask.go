package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
)

// hwSubtasksMsg reports the subtasks created under a Hasaki Work task.
type hwSubtasksMsg struct {
	parent string   // code of the parent task
	codes  []string // created, in order
	err    error    // stopped at the first failure
	warn   string   // created but not assigned
}

// subtaskNames splits the form's text: one subtask per non-empty line
// (leading list markers removed).
func subtaskNames(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-*•0123456789.) "))
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// subtaskDeadline is the parent's deadline when it is still ahead, else today.
func subtaskDeadline(parent hasakiwork.Task, now time.Time) string {
	if len(parent.DateEnd) >= 10 {
		if d, err := time.ParseInLocation("2006-01-02", parent.DateEnd[:10], time.Local); err == nil && !d.Before(now.Truncate(24*time.Hour)) {
			return d.Format("2006-01-02")
		}
	}
	return now.Format("2006-01-02")
}

// subtaskForm creates subtasks under a Hasaki Work task, as the web app's
// subtask list does: same project and sprint as the parent, assigned to the
// operator. This WRITES to Hasaki Work.
func (m Model) subtaskForm(parent hasakiwork.Task) (tea.Model, tea.Cmd) {
	if m.d.HW == nil {
		m.msg = "chưa đăng nhập Hasaki Work: vào tab 4 Status và bấm l"
		return m, nil
	}
	if parent.ProjectID == 0 {
		m.msg = parent.Code + " không thuộc dự án nào trên Hasaki Work: không tạo subtask được"
		return m, nil
	}
	hw := m.d.HW
	f := newForm("Subtask của "+parent.Code+" — "+truncate(parent.Name, 50), func(v map[string]string) (tea.Cmd, error) {
		names := subtaskNames(v["names"])
		if len(names) == 0 {
			return nil, fmt.Errorf("nhập ít nhất một subtask")
		}
		if len(names) > 30 {
			return nil, fmt.Errorf("tối đa 30 subtask mỗi lần (đang có %d)", len(names))
		}
		base, err := parseNewHWTask(map[string]string{"name": names[0], "hours": v["hours"], "deadline": v["deadline"]}, parent.ProjectID)
		if err != nil {
			return nil, err
		}
		assign := v["assign"] == "me"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			msg := hwSubtasksMsg{parent: parent.Code}
			var staffID int64
			if assign {
				if p, err := hw.Profile(ctx); err != nil || p.StaffID == 0 {
					msg.warn = "không đọc được staff id, subtask chưa được giao"
				} else {
					staffID = p.StaffID
				}
			}
			for _, name := range names {
				nt := base
				nt.Name, nt.ParentID, nt.MilestoneID = name, parent.ID, int64(parent.MilestoneID)
				c, err := hw.CreateTask(ctx, nt)
				if err != nil {
					msg.err = fmt.Errorf("tạo %q: %w", name, err)
					return msg
				}
				msg.codes = append(msg.codes, c.Code)
				if staffID != 0 {
					if err := hw.AssignStaff(ctx, c.ID, staffID); err != nil && msg.warn == "" {
						msg.warn = "giao cho bạn lỗi: " + err.Error()
					}
				}
			}
			return msg
		}, nil
	})
	f.hint = fmt.Sprintf("Ghi thật lên Hasaki Work: subtask nằm dưới %s, cùng dự án và sprint. Mỗi dòng một subtask.", parent.Code)
	f.addArea("names", "Tên subtask (mỗi dòng một cái)", "", true)
	f.addText("hours", "Giờ dự kiến mỗi subtask", "4", false)
	f.addText("deadline", "Deadline (YYYY-MM-DD)", subtaskDeadline(parent, time.Now()), false)
	f.addSelect("assign", "Giao cho", func(*form) []option {
		return []option{{"tôi", "me"}, {"chưa giao", ""}}
	}, "me")
	m.modal = f
	m.msg = "tạo subtask cho " + parent.Code + " (Ctrl+S gửi, Esc huỷ)"
	return m, f.start()
}

func (m Model) onHWSubtasks(msg hwSubtasksMsg) (tea.Model, tea.Cmd) {
	created := ""
	if len(msg.codes) > 0 {
		created = fmt.Sprintf("đã tạo %d subtask dưới %s: %s", len(msg.codes), msg.parent, strings.Join(msg.codes, ", "))
	}
	switch {
	case msg.err != nil && created != "":
		m.msg = created + "; dừng ở lỗi: " + msg.err.Error()
	case msg.err != nil:
		m.msg = "tạo subtask: " + msg.err.Error()
	default:
		m.msg = created
	}
	if msg.warn != "" {
		m.msg += " (" + msg.warn + ")"
	}
	if len(msg.codes) == 0 {
		return m, nil
	}
	return m.loadHW() // the new subtasks show in the list
}

// subtaskFromLocal opens the subtask form for the Hasaki Work task a local
// task came from (key h in the Tasks tab).
func (m Model) subtaskFromLocal(code string) (tea.Model, tea.Cmd) {
	for _, t := range m.hw.list {
		if t.Code == code {
			if full, ok := m.hw.details[t.ID]; ok {
				return m.subtaskForm(*full)
			}
			return m.subtaskForm(t)
		}
	}
	m.msg = code + " không có trong danh sách Hasaki Work đang tải: mở tab 2 (đổi bộ lọc bằng s) rồi bấm n trên task đó"
	return m, nil
}
