package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Taking back what the operator sent (key u), at any time:
//   - a message the agent has not received yet is deleted;
//   - once a run started from a message or a decision, the daemon stops the
//     run if it is still going, takes it back and rewinds the agent's
//     session to before it (the agent never saw it). The form reopens with
//     the text, to fix and send again.

// lastSent is the latest run started by what the operator sent, not taken
// back, and what that was.
func (m Model) lastSent(t *store.Task) (*store.RunPrompt, *store.RunTrigger) {
	runs, err := m.d.Store.RunPrompts(t.ID)
	if err != nil {
		return nil, nil
	}
	for i := len(runs) - 1; i >= 1; i-- {
		r := runs[i]
		if r.Revoked {
			continue
		}
		switch r.Trigger {
		case "operator_message", "approval", "rejection":
		default:
			return nil, nil
		}
		tr, err := m.d.Store.TriggerOf(t.ID, runs[i-1].StartedAt, r)
		if err != nil {
			return nil, nil
		}
		return r, tr
	}
	return nil, nil
}

// undoNotice is the line telling a take-back is in progress, or that a
// message is waiting to be sent ("" otherwise).
func (m Model) undoNotice(t *store.Task) string {
	if t == nil {
		return ""
	}
	if at := m.d.Store.UndoRequestedAt(t.ID); !at.IsZero() {
		if time.Since(at) > 15*time.Second {
			return fmt.Sprintf("↶ Yêu cầu thu hồi chờ từ %s mà daemon chưa xử lý: daemon có đang chạy bản mới không? "+
				"(khởi động lại daemon sau khi cài bản mới)", at.Local().Format("15:04:05"))
		}
		return "↶ Đang thu hồi lượt cuối: daemon dừng agent (nếu đang chạy) rồi tua session về trước lượt đó…"
	}
	if pm, _ := m.d.Store.PendingMessage(t.ID); pm != nil {
		return "✉ Tin nhắn chưa tới agent · u thu hồi để sửa"
	}
	return ""
}

// undoSend takes back what the operator sent last.
func (m Model) undoSend(t *store.Task) (tea.Model, tea.Cmd) {
	if t == nil {
		return m, nil
	}
	if text, err := m.d.Store.RevokeMessage(t.ID); err != nil {
		m.msg = "thu hồi: " + err.Error()
		return m, nil
	} else if text != "" {
		m.reloadTasks()
		return m.messageForm(t, text, "Đã thu hồi tin nhắn (agent chưa nhận) — sửa rồi Ctrl+S để gửi lại, Esc để bỏ hẳn.")
	}
	if m.d.Store.UndoRequested(t.ID) {
		m.msg = "đang thu hồi rồi, đợi daemon xử lý"
		return m, nil
	}
	if m.d.Store.TakenOver(t.ID) {
		m.msg = "task đang được làm tay (o): thoát phiên đó trước"
		return m, nil
	}
	r, tr := m.lastSent(t)
	if r == nil {
		m.msg = "không có tin nhắn hay quyết định nào để thu hồi"
		return m, nil
	}
	var b strings.Builder
	what := "tin nhắn"
	if tr.Request != "" {
		what = map[bool]string{true: "quyết định duyệt " + tr.Request, false: "quyết định từ chối " + tr.Request}[tr.Approved]
	}
	fmt.Fprintf(&b, "Thu hồi %s đã bắt đầu lượt %d (%s)?\n\n", what, r.N, r.StartedAt.Local().Format("02/01 15:04"))
	if sent := strings.TrimSpace(strings.Join(append(tr.Messages, tr.Comment), "\n")); sent != "" {
		b.WriteString(dimStyle.Render(truncate(strings.ReplaceAll(sent, "\n", " ⏎ "), 300)) + "\n\n")
	}
	if t.State == store.StateRunning || t.State == store.StatePreparing {
		b.WriteString("• Agent đang chạy lượt này: sẽ bị dừng.\n")
	}
	b.WriteString("• Session của agent tua về trước lượt này: agent không còn thấy nội dung đó.\n")
	b.WriteString("• Thay đổi agent đã làm trong worktree ở lượt đó (file, commit) vẫn còn; PR đã mở / đã merge không bị huỷ.\n")
	b.WriteString("Sau đó form mở lại với nội dung cũ để bạn sửa và gửi lại.")
	task := *t
	m.conf = &confirm{text: b.String(), onYes: func() tea.Cmd {
		return func() tea.Msg { return undoRequestedMsg{task: &task, trigger: tr, err: m.d.Store.RequestUndo(task.ID)} }
	}}
	return m, nil
}

// undoRequestedMsg follows the operator's confirmation of a take-back.
type undoRequestedMsg struct {
	task    *store.Task
	trigger *store.RunTrigger
	err     error
}

func (m Model) onUndoRequested(msg undoRequestedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.msg = "thu hồi: " + msg.err.Error()
		return m, nil
	}
	m.reloadTasks()
	tr := msg.trigger
	if tr.Request != "" {
		return m.decisionForm(msg.task, tr.Request, tr.Approved, tr.Comment)
	}
	return m.messageForm(msg.task, strings.Join(tr.Messages, "\n\n"),
		"Đang thu hồi tin nhắn cũ — sửa rồi Ctrl+S: tin mới tới agent sau khi thu hồi xong. Esc: không gửi gì.")
}

// messageForm is the form that messages a stopped task's agent.
func (m Model) messageForm(t *store.Task, initial, hint string) (tea.Model, tea.Cmd) {
	id := t.ID
	f := newForm("Nhắn agent — "+truncate(t.Title, 60), func(v map[string]string) (tea.Cmd, error) {
		text, err := m.attach(id, v["text"])
		if err != nil {
			return nil, err
		}
		return nil, m.d.Store.SetNudge(id, text)
	})
	f.hint = hint
	f.addArea("text", "Tin nhắn", initial, true)
	m.modal = f
	return m, f.start()
}

// decisionForm decides request reqID again, once the daemon has taken the
// old decision back.
func (m Model) decisionForm(t *store.Task, reqID string, approve bool, initial string) (tea.Model, tea.Cmd) {
	id := t.ID
	verb := map[bool]string{true: "Duyệt", false: "Từ chối / góp ý"}[approve]
	f := newForm(fmt.Sprintf("%s lại %s — %s", verb, reqID, truncate(t.Title, 50)), func(v map[string]string) (tea.Cmd, error) {
		req, err := m.d.Store.PendingRequest(id)
		if err != nil {
			return nil, err
		}
		if req == nil || req.ID != reqID {
			if m.d.Store.UndoRequested(id) {
				return nil, fmt.Errorf("daemon chưa thu hồi xong (đợi agent dừng); thử Ctrl+S lại sau vài giây")
			}
			return nil, fmt.Errorf("%s không còn chờ quyết định", reqID)
		}
		text, err := m.attach(id, v["text"])
		if err != nil {
			return nil, err
		}
		return nil, m.d.Store.Decide(id, req, approve, text)
	})
	f.hint = "Quyết định cũ đang được thu hồi — sửa rồi Ctrl+S để gửi lại. Esc: yêu cầu chờ bạn (a/x/m như thường)."
	f.addArea("text", "Nội dung gửi agent", initial, !approve)
	m.modal = f
	return m, f.start()
}
