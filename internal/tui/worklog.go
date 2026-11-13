package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

// workDraftMsg brings the drafted name and result of a report.
type workDraftMsg struct {
	taskID string
	draft  *worklog.Draft
	err    error
}

// workInput gathers what taskpilot knows about the work for the drafter.
func (m Model) workInput(t *store.Task, parent string) worklog.Input {
	st := m.d.Store
	in := worklog.Input{Language: m.d.Config.OperatorLanguage, Title: t.Title, Ticket: t.Body, Parent: parent, Subtask: parent != ""}
	p := m.d.Config.Projects[t.Project]
	in.Project = p.Name
	if c, _ := st.Completion(t.ID); c != nil {
		in.Completion = c.Summary
	} else if t.State == store.StateDone {
		in.Completion = t.LastResult
	}
	if prs, _ := st.PullRequests(t.ID); len(prs) > 0 {
		for _, pr := range sortedPRs(prs) {
			if pr.Pending {
				continue
			}
			in.PRs = append(in.PRs, fmt.Sprintf("%s → %s: %s — %s %s", pr.Repo, pr.Destination, pr.Title, pr.URL, truncate(pr.Description, 300)))
		}
	}
	reqs, _ := st.Requests(t.ID)
	decs, _ := st.Decisions(t.ID)
	for _, r := range reqs {
		line := r.ID + ": " + r.Summary
		if d, ok := decs[r.ID]; ok {
			line += map[bool]string{true: " (duyệt)", false: " (từ chối)"}[d.Approved]
			if c := strings.TrimSpace(d.Comment); c != "" {
				line += " | operator: " + c
			}
		}
		in.Gates = append(in.Gates, line)
	}
	for _, x := range m.hw.list {
		if p.HasakiProjectID != 0 && x.ProjectID == p.HasakiProjectID && !hasakiwork.IsEpic(x.Name) && len(in.Examples) < 15 {
			in.Examples = append(in.Examples, x.Name)
		}
	}
	return in
}

// draftWork starts drafting the report of t (cached per task once done).
func (m Model) draftWork(t *store.Task, parent string) (tea.Model, tea.Cmd) {
	if m.tasks.workDrafting == nil {
		m.tasks.workDrafting = map[string]bool{}
	}
	if m.tasks.workDrafting[t.ID] {
		m.msg = "Claude đang soạn tên và kết quả công việc…"
		return m, nil
	}
	m.tasks.workDrafting[t.ID] = true
	m.msg = "Claude đang soạn tên và kết quả công việc từ ticket, tóm tắt hoàn thành, PR và lịch sử duyệt…"
	gen, in, id := m.d.WorkLog, m.workInput(t, parent), t.ID
	return m, func() tea.Msg {
		d, err := gen(context.Background(), in)
		return workDraftMsg{taskID: id, draft: d, err: err}
	}
}

func (m Model) onWorkDraft(msg workDraftMsg) (tea.Model, tea.Cmd) {
	delete(m.tasks.workDrafting, msg.taskID)
	if m.tasks.workDrafts == nil {
		m.tasks.workDrafts = map[string]*workDraftMsg{}
	}
	m.tasks.workDrafts[msg.taskID] = &msg
	t := m.tasks.current()
	if t == nil || t.ID != msg.taskID || m.modal != nil || m.tab != tabTasks {
		m.msg = "đã soạn xong tên và kết quả công việc: bấm h lần nữa để xem"
		return m, nil
	}
	return m.pushToHW(t)
}

// workDraft returns the finished draft of t, if any: (nil, "") when none,
// (nil, reason) when drafting failed.
func (m Model) workDraft(t *store.Task) (*worklog.Draft, string) {
	d := m.tasks.workDrafts[t.ID]
	if d == nil {
		return nil, ""
	}
	if d.err != nil {
		return nil, d.err.Error()
	}
	return d.draft, ""
}

// needDraft reports whether h should first draft the report of t.
func (m Model) needDraft(t *store.Task) bool {
	return m.d.WorkLog != nil && m.tasks.workDrafts[t.ID] == nil
}

// draftNote is the hint line about the draft.
func draftNote(errText string, drafted bool) string {
	switch {
	case errText != "":
		return "\nClaude không soạn được (" + truncate(errText, 120) + "): dùng nội dung mặc định."
	case drafted:
		return "\nTên và kết quả do Claude soạn từ ticket, tóm tắt hoàn thành, PR và lịch sử duyệt: đọc lại và sửa nếu cần."
	}
	return ""
}
