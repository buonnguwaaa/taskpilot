package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Attaching a Hasaki Work task to a taskpilot task that already runs (key a
// on the Hasaki Work tab): a bug of the feature a task builds is often its
// own ticket, but the work belongs in the same session and branch. The
// ticket (description, files, prototypes) goes to the task's agent as an
// operator message, and the ticket is marked as worked in that task.

// hwHost returns the taskpilot task a Hasaki Work task is worked in: its own
// task, or the one it was attached to; "" when none.
func (m Model) hwHost(code string) string {
	if code == "" {
		return ""
	}
	if id := store.TaskID(hwSource, code); m.d.Store.Exists(id) {
		return id
	}
	if id, _ := m.d.Store.TicketHost(hwSource, code); id != "" && m.d.Store.Exists(id) {
		return id
	}
	return ""
}

// hostOptions lists the tasks a ticket can be attached to: the tasks of the
// ticket's project first, then the others; in each, those not done first,
// most recently updated first.
func (m Model) hostOptions(project string) func(*form) []option {
	return func(*form) []option {
		all, _ := m.d.Store.ListTasks()
		rank := func(t *store.Task) int {
			r := 0
			if t.Project != project {
				r += 2
			}
			if isDone(t) {
				r++
			}
			return r
		}
		sort.SliceStable(all, func(i, j int) bool {
			if a, b := rank(all[i]), rank(all[j]); a != b {
				return a < b
			}
			return all[i].UpdatedAt.After(all[j].UpdatedAt)
		})
		var out []option
		for _, t := range all {
			label := truncate(t.Title, 60) + "  · " + lookOf(t.State).label + " · " + t.ID
			out = append(out, option{Label: label, Value: t.ID})
		}
		return out
	}
}

// attachHW opens the form that attaches the selected Hasaki Work task to a
// task already running.
func (m Model) attachHW() (tea.Model, tea.Cmd) {
	t := m.hw.current()
	if t == nil {
		return m, nil
	}
	if host := m.hwHost(t.Code); host != "" {
		m.msg = t.Code + " đã được làm trong task " + host
		return m, nil
	}
	full, ok := m.hw.details[t.ID]
	if !ok {
		m.msg = "đang tải mô tả task… bấm a lại khi tải xong"
		return m.selectHW()
	}
	if len(m.hostOptions("")(nil)) == 0 {
		m.msg = "chưa có task nào để gộp vào: Enter để giao thành task mới"
		return m, nil
	}
	initial := ""
	if cur := m.tasks.current(); cur != nil {
		initial = cur.ID // the task open on tab 1
	}
	task, hw, fetchArt, st := *full, m.d.HW, m.d.ClaudeArtifact, m.d.Store
	f := newForm("Gộp "+t.Code+" vào task đang làm", func(v map[string]string) (tea.Cmd, error) {
		host := v["host"]
		ht, err := st.LoadTask(host)
		if err != nil {
			return nil, err
		}
		notes, err := m.attach(host, v["notes"])
		if err != nil {
			return nil, err
		}
		dir := refs.Dir(st.Home, host)
		return func() tea.Msg {
			files := hwDownloadFiles(hw, &task, dir)
			files = append(files, fetchArtifacts(fetchArt, task.Note+"\n"+notes, dir)...)
			msg := attachMessage(ht, hwTicketBody(&task, notes, files))
			if err := st.SetNudge(host, msg); err != nil {
				return infoMsg("gộp " + task.Code + " thất bại: " + err.Error())
			}
			if err := st.AttachTicket(&store.TaskTicket{TaskID: host, Source: hwSource, TicketID: task.Code, Title: task.Name}); err != nil {
				return infoMsg("đã gửi " + task.Code + " cho agent nhưng không lưu được liên kết: " + err.Error())
			}
			out := fmt.Sprintf("đã gộp %s vào task %s: agent nhận ở lượt chạy tới", task.Code, host)
			failed := 0
			for _, f := range files {
				if f.err != nil {
					failed++
				}
			}
			if failed > 0 {
				out += fmt.Sprintf(" (%d tệp không tải được)", failed)
			}
			return infoMsg(out)
		}, nil
	})
	f.hint = truncate(t.Name, 120) + "\nAgent của task được chọn nhận toàn bộ ticket (mô tả, tệp, prototype) như một tin nhắn và làm tiếp trên cùng session, cùng branch."
	f.addSelect("host", "Gộp vào task", m.hostOptions(m.scope(t)), initial)
	f.addArea("notes", "Ghi chú thêm cho agent (không bắt buộc)", "", false)
	m.modal = f
	m.msg = "gộp " + t.Code + " vào một task"
	return m, f.start()
}

// attachMessage is what the host task's agent reads.
func attachMessage(host *store.Task, body string) string {
	var b strings.Builder
	b.WriteString("Another ticket is attached to this task: work on it in this same task, on the current branch")
	if host.Branch != "" {
		b.WriteString(" (" + host.Branch + ")")
	}
	b.WriteString(". It is usually a bug or a follow-up of what this task builds. Treat it like the task's own requirement: plan the change (or extend the current plan), keep the gates as usual, and say in your summaries which ticket each change addresses.\n\n")
	b.WriteString(body)
	return b.String()
}
