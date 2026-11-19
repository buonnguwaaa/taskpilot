package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// togglePostpone puts the selected task on hold, or takes it off hold (key z).
// On hold only moves the task to the "Tạm hoãn" group and out of the "cần
// bạn" count: the agent and a pending request are left as they are, and the
// task leaves the group by itself once it runs again (a message, an approval)
// or is marked done.
func (m Model) togglePostpone(t *store.Task) (tea.Model, tea.Cmd) {
	switch {
	case t == nil:
		return m, nil
	case m.tasks.postponed[t.ID]:
		if err := m.d.Store.Resume(t.ID); err != nil {
			m.msg = "bỏ tạm hoãn: " + err.Error()
			return m, nil
		}
		m.msg = "đã đưa " + t.ID + " về Đang làm"
	case isDone(t):
		m.msg = "task đã xong, không tạm hoãn được"
		return m, nil
	case t.State == store.StateRunning || t.State == store.StatePreparing || t.State == store.StateQueued:
		m.msg = "task đang chạy hoặc sắp chạy (" + string(t.State) + "): đợi nó dừng rồi mới tạm hoãn"
		return m, nil
	default:
		if err := m.d.Store.Postpone(t.ID, ""); err != nil {
			m.msg = "tạm hoãn: " + err.Error()
			return m, nil
		}
		m.msg = "đã tạm hoãn " + t.ID + " (z lần nữa để làm tiếp)"
	}
	m.reloadTasks()
	return m, nil
}
