package tui

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/intake"
)

// draftStartedMsg marks that a prompt was sent to Claude.
type draftStartedMsg struct{}

// draftMsg carries the result of turning a prompt into a ticket draft.
type draftMsg struct {
	prompt string
	draft  *intake.Draft
	err    error
}

// promptTaskForm asks for a free-form request. Claude drafts the ticket in
// the background; the draft then opens in the regular task form, so the
// operator reviews project, repo, title and body before anything is queued.
func (m Model) promptTaskForm(initial, errText string) (tea.Model, tea.Cmd) {
	if m.tasks.draft != nil {
		d := *m.tasks.draft
		m.tasks.draft = nil
		return m.onDraft(d)
	}
	if m.d.Draft == nil {
		m.msg = "không có Claude để soạn task từ prompt; bấm n để tạo bằng form"
		return m, nil
	}
	if m.d.Inbox == nil {
		m.msg = "không có inbox để tạo task"
		return m, nil
	}
	if m.tasks.drafting {
		m.msg = "Claude vẫn đang soạn task từ prompt trước…"
		return m, nil
	}
	draft := m.d.Draft
	f := newForm("Task từ prompt", func(v map[string]string) (tea.Cmd, error) {
		prompt := v["prompt"]
		if prompt == "" {
			return nil, errors.New("prompt trống")
		}
		return tea.Sequence(
			func() tea.Msg { return draftStartedMsg{} },
			func() tea.Msg {
				d, err := draft(context.Background(), prompt)
				return draftMsg{prompt: prompt, draft: d, err: err}
			}), nil
	})
	f.hint = "Mô tả việc cần làm bằng lời: lỗi gì, ở đâu, kết quả mong muốn. " +
		"Claude chọn project, repo và viết tiêu đề + mô tả; bạn xem lại trước khi gửi."
	f.addArea("prompt", "Bạn cần agent làm gì?", initial, true)
	f.err = errText
	m.modal = f
	return m, f.start()
}

// onDraft opens the drafted ticket in the task form, or the prompt again on error.
func (m Model) onDraft(msg draftMsg) (tea.Model, tea.Cmd) {
	m.tasks.drafting = false
	if m.modal != nil || m.conf != nil {
		// Don't replace what the operator is typing; "N" shows it later.
		m.tasks.draft = &msg
		m.msg = "Claude đã soạn xong task từ prompt — bấm N ở tab Tasks để xem"
		return m, nil
	}
	m.tab = tabTasks
	if msg.err != nil {
		m.msg = "soạn task: " + msg.err.Error()
		return m.promptTaskForm(msg.prompt, msg.err.Error())
	}
	hint := "Claude đã soạn từ prompt của bạn — kiểm tra rồi Ctrl+S để gửi."
	if msg.draft.Note != "" {
		hint = "Claude: " + msg.draft.Note + "\n" + hint
	}
	m.msg = "bản nháp: " + truncate(msg.draft.Title, 60)
	return m.newTaskForm(msg.draft.Ticket(msg.prompt), hint)
}
