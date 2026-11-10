package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// manualDoneNote is the completion summary when the operator writes none.
const manualDoneNote = "Đánh dấu xong bằng tay."

// markDone closes a parked task by hand (the work was finished outside the
// agent, or is not needed any more): state done, a completion summary, and
// the pending approval request, if any, closed. The workspace stays.
func (m Model) markDone(t *store.Task) (tea.Model, tea.Cmd) {
	switch {
	case t == nil:
		return m, nil
	case t.State == store.StateDone:
		m.msg = "task này đã xong rồi"
		return m, nil
	case t.State == store.StateRunning || t.State == store.StatePreparing || t.State == store.StateQueued:
		m.msg = "task đang chạy hoặc sắp chạy (" + string(t.State) + "): đợi nó dừng rồi mới đánh dấu xong"
		return m, nil
	case m.d.Store.TakenOver(t.ID):
		m.msg = "bạn đang làm tay task này: thoát session claude đó rồi bấm c"
		return m, nil
	}
	st, id := m.d.Store, t.ID
	ws := t.Workspace
	f := newForm("Đánh dấu xong — "+truncate(t.Title, 60), func(v map[string]string) (tea.Cmd, error) {
		if err := closeTask(st, id, v["summary"]); err != nil {
			return nil, err
		}
		return func() tea.Msg { return releaseMsg(id, ws) }, nil
	})
	f.hint = "Task chuyển sang done; agent không chạy tiếp. Worktree sạch được gỡ để bạn checkout được nhánh của task trong repo của mình;" +
		" worktree còn thay đổi chưa commit thì giữ lại. Nhánh và commit vẫn còn nguyên.\nGhi chú dùng làm tóm tắt hoàn thành (và mô tả khi bấm h đẩy lên Hasaki Work)."
	if m.tasks.pending != nil && m.tasks.current() == t {
		f.hint += fmt.Sprintf("\nYêu cầu %s đang chờ duyệt sẽ được đóng.", m.tasks.pending.ID)
	}
	f.addArea("summary", "Đã làm gì (không bắt buộc)", "", false)
	m.modal = f
	return m, f.start()
}

// releaseMsg frees the clean worktrees of a task marked done and reports it.
func releaseMsg(id, ws string) tea.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	kept, err := workspace.Release(ctx, ws)
	switch {
	case err != nil:
		return infoMsg("đã đánh dấu xong " + id + ", nhưng gỡ worktree lỗi: " + err.Error())
	case len(kept) > 0:
		return infoMsg(fmt.Sprintf("đã đánh dấu xong %s; giữ worktree còn thay đổi chưa commit: %s", id, strings.Join(kept, ", ")))
	}
	return infoMsg("đã đánh dấu xong " + id + " và trả nhánh lại (gỡ worktree)")
}

// closeTask moves a parked task to done. The state changes first: once done,
// the daemon no longer acts on the task, so closing its pending request
// afterwards cannot be read as a rejection that resumes the agent.
func closeTask(st *store.Store, id, summary string) error {
	t, err := st.LoadTask(id)
	if err != nil {
		return err
	}
	switch t.State {
	case store.StateRunning, store.StatePreparing, store.StateQueued:
		return fmt.Errorf("task vừa chuyển sang %s, không đánh dấu xong được", t.State)
	case store.StateDone:
		return nil
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		summary = manualDoneNote
	}
	t.LastError = ""
	t.Transition(store.StateDone, "closed by the operator: "+firstLine(summary))
	if err := st.SaveTask(t); err != nil {
		return err
	}
	if req, err := st.PendingRequest(id); err != nil {
		return err
	} else if req != nil {
		if err := st.Decide(id, req, false, "closed: task marked done by the operator"); err != nil {
			return err
		}
	}
	return st.SetCompletion(id, &store.Completion{Summary: summary, CreatedAt: time.Now()})
}
