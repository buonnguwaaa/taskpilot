package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/refs"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// taskDeletedMsg reports the end of a task deletion.
type taskDeletedMsg struct {
	id  string
	err error
	// warn lists cleanups that failed after the task itself was deleted.
	warn []string
}

// deleteTask asks for confirmation, then deletes the selected task: its
// record and history, its workspace (worktrees, uncommitted changes lost) and
// its inbox ticket, so the daemon does not pick it up again. The work branches
// stay in the repositories; nothing is changed on Hasaki Work or Bitbucket.
func (m Model) deleteTask(t *store.Task) (tea.Model, tea.Cmd) {
	switch {
	case t == nil:
		return m, nil
	case t.State == store.StateRunning || t.State == store.StatePreparing:
		m.msg = "task đang chạy: đợi nó dừng (hoặc dừng daemon) rồi mới xoá"
		return m, nil
	case m.d.Store.TakenOver(t.ID):
		m.msg = "task đang được làm tay (takeover): thoát session đó rồi mới xoá"
		return m, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Xoá task %s?\n%s\n\n", t.ID, truncate(t.Title, 80))
	b.WriteString("Sẽ xoá: lịch sử, lượt chạy, yêu cầu duyệt, PR đã ghi nhận")
	if t.Workspace != "" {
		fmt.Fprintf(&b, ",\nworkspace %s (mọi thay đổi CHƯA commit sẽ mất)", t.Workspace)
	}
	b.WriteString(".\n")
	if t.Branch != "" {
		fmt.Fprintf(&b, "Giữ lại: nhánh %s trong các repo, PR trên Bitbucket", t.Branch)
	} else {
		b.WriteString("Giữ lại: PR trên Bitbucket")
	}
	if m.tasks.hwLink != nil && m.tasks.current() == t {
		fmt.Fprintf(&b, ", task %s trên Hasaki Work", m.tasks.hwLink.Code)
	}
	b.WriteString(".")
	task, st, inbox := *t, m.d.Store, m.d.Inbox
	m.conf = &confirm{text: b.String(), onYes: func() tea.Cmd {
		return func() tea.Msg {
			// The record goes first: once it is gone the daemon no longer
			// schedules the task, and the ticket must go before the next poll.
			if err := st.DeleteTask(task.ID); err != nil {
				return taskDeletedMsg{id: task.ID, err: err}
			}
			var warn []string
			if err := os.RemoveAll(refs.Dir(st.Home, task.ID)); err != nil {
				warn = append(warn, "xoá tệp đính kèm: "+err.Error())
			}
			if inbox != nil {
				if err := inbox.Remove(task.TicketID); err != nil {
					warn = append(warn, "xoá ticket trong inbox: "+err.Error())
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := workspace.Remove(ctx, task.Workspace); err != nil {
				warn = append(warn, err.Error())
			}
			return taskDeletedMsg{id: task.ID, warn: warn}
		}
	}}
	return m, nil
}

func (m Model) onTaskDeleted(msg taskDeletedMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.err != nil:
		m.msg = "xoá " + msg.id + ": " + msg.err.Error()
	case len(msg.warn) > 0:
		m.msg = "đã xoá " + msg.id + ", nhưng: " + strings.Join(msg.warn, "; ")
	default:
		m.msg = "đã xoá " + msg.id
	}
	m.reloadTasks()
	return m, nil
}
