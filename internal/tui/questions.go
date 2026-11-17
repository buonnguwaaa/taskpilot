package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// otherAnswer is the option value of "write another answer".
const otherAnswer = "\x00other"

// isQuestion reports whether the request only asks questions (ask_operator,
// or an AskUserQuestion the hook forwarded) rather than opening a gate.
func isQuestion(r *store.ApprovalRequest) bool {
	return r != nil && r.Gate == store.QuestionGate
}

// answerText writes the operator's answers for the agent: each question with
// the chosen option(s) and any free text.
func answerText(qs []store.Question, picks [][]string, others []string, note string) string {
	var b strings.Builder
	for i, q := range qs {
		fmt.Fprintf(&b, "Q%d: %s\n", i+1, q.Question)
		var chosen []string
		for _, p := range picks[i] {
			if p != otherAnswer {
				chosen = append(chosen, p)
			}
		}
		if other := strings.TrimSpace(others[i]); other != "" {
			chosen = append(chosen, "Khác: "+other)
		}
		if len(chosen) == 0 {
			chosen = []string{"(không chọn)"}
		}
		fmt.Fprintf(&b, "→ %s\n", strings.Join(chosen, "; "))
	}
	if n := strings.TrimSpace(note); n != "" {
		fmt.Fprintf(&b, "\nGhi chú thêm: %s\n", n)
	}
	return strings.TrimRight(b.String(), "\n")
}

// answerForm answers every question (one option, several, or another
// answer) and, for a gate request, approves or rejects it at the same time.
func (m Model) answerForm(t *store.Task, r *store.ApprovalRequest, approve bool) (tea.Model, tea.Cmd) {
	id, qs := t.ID, r.Questions
	gate := !isQuestion(r)
	f := newForm(fmt.Sprintf("Trả lời %s — %s", r.ID, truncate(t.Title, 50)), func(v map[string]string) (tea.Cmd, error) {
		picks := make([][]string, len(qs))
		others := make([]string, len(qs))
		for i, q := range qs {
			key := "q" + strconv.Itoa(i)
			if q.Multi {
				picks[i] = multiValues(v[key])
			} else {
				picks[i] = []string{v[key]}
			}
			others[i] = v["o"+strconv.Itoa(i)]
			wantsOther := false
			for _, p := range picks[i] {
				wantsOther = wantsOther || p == otherAnswer
			}
			if wantsOther && strings.TrimSpace(others[i]) == "" {
				return nil, fmt.Errorf("câu %d: đã chọn \"Khác\" thì gõ câu trả lời ở ô bên dưới", i+1)
			}
			if q.Multi && len(picks[i]) == 0 && strings.TrimSpace(others[i]) == "" {
				return nil, fmt.Errorf("câu %d: đánh dấu ít nhất một lựa chọn (Space) hoặc gõ câu trả lời khác", i+1)
			}
		}
		text, err := m.attach(id, answerText(qs, picks, others, v["note"]))
		if err != nil {
			return nil, err
		}
		ok := true
		if gate {
			ok = v["decision"] == "approve"
		}
		return nil, m.d.Store.Decide(id, r, ok, text)
	})
	f.hint = wrap(r.Summary, 100)
	if gate {
		f.addSelect("decision", "Quyết định cho cổng "+r.Gate, func(*form) []option {
			return []option{{"Duyệt", "approve"}, {"Từ chối (agent sửa theo câu trả lời)", "reject"}}
		}, map[bool]string{true: "approve", false: "reject"}[approve])
	}
	for i, q := range qs {
		opts := func(q store.Question) func(*form) []option {
			return func(*form) []option {
				var o []option
				for _, x := range q.Options {
					o = append(o, option{Label: x.Label, Value: x.Label})
				}
				return append(o, option{Label: "Khác…", Value: otherAnswer})
			}
		}(q)
		label := q.Question
		if len(qs) > 1 {
			label = fmt.Sprintf("%d. %s", i+1, q.Question)
		}
		if q.Multi {
			f.addMulti("q"+strconv.Itoa(i), label+" (Space để chọn nhiều)", opts, false)
		} else {
			f.addSelect("q"+strconv.Itoa(i), label, opts, q.Options[0].Label)
		}
		f.addText("o"+strconv.Itoa(i), "Câu trả lời khác / bổ sung (không bắt buộc)", "", false)
	}
	f.addArea("note", "Ghi chú cho agent (không bắt buộc; @tệp, Ctrl+O hoặc Ctrl+V/Alt+V dán ảnh để đính kèm)", "", false)
	m.modal = f
	m.msg = "trả lời " + r.ID + " (Ctrl+S gửi)"
	return m, f.start()
}

// replyForm sends a free reply to a parked task: an answer to a question
// request, or feedback on a gate request without approving it.
func (m Model) replyForm(t *store.Task, r *store.ApprovalRequest) (tea.Model, tea.Cmd) {
	id := t.ID
	title := "Trả lời " + r.ID
	label := "Câu trả lời (agent chạy tiếp theo câu này)"
	approve := true
	if !isQuestion(r) {
		title = "Góp ý cho " + r.ID + " (chưa duyệt)"
		label = "Góp ý / câu trả lời (agent sửa rồi xin duyệt lại)"
		approve = false
	}
	f := newForm(title+" — "+truncate(t.Title, 50), func(v map[string]string) (tea.Cmd, error) {
		text, err := m.attach(id, v["text"])
		if err != nil {
			return nil, err
		}
		return nil, m.d.Store.Decide(id, r, approve, text)
	})
	f.hint = wrap(r.Summary, 100)
	f.addArea("text", label, "", true)
	m.modal = f
	return m, f.start()
}

// inlineAnswer is the operator's choices for the pending request's
// questions, made in the detail pane (digits, ←/→, 0, Enter) before sending.
type inlineAnswer struct {
	reqID  string
	cur    int            // the question the digits answer
	picks  []map[int]bool // option indexes chosen, per question
	others []string       // "Khác" answers, per question
}

// answers returns the inline state for r, starting it if needed.
func (m *Model) answers(r *store.ApprovalRequest) *inlineAnswer {
	a := m.tasks.answer
	if a == nil || a.reqID != r.ID || len(a.picks) != len(r.Questions) {
		a = &inlineAnswer{reqID: r.ID, picks: make([]map[int]bool, len(r.Questions)), others: make([]string, len(r.Questions))}
		for i := range a.picks {
			a.picks[i] = map[int]bool{}
		}
		m.tasks.answer = a
	}
	return a
}

// complete reports whether every question has an answer.
func (a *inlineAnswer) complete() bool {
	for i := range a.picks {
		if len(a.picks[i]) == 0 && strings.TrimSpace(a.others[i]) == "" {
			return false
		}
	}
	return true
}

// text composes the answers for the agent.
func (a *inlineAnswer) text(qs []store.Question) string {
	picks := make([][]string, len(qs))
	for i, q := range qs {
		for j, o := range q.Options {
			if a.picks[i][j] {
				picks[i] = append(picks[i], o.Label)
			}
		}
	}
	return answerText(qs, picks, a.others, "")
}

// inlineKey handles the answer keys of a pending request with questions;
// ok=false lets the key through. Digits pick (several on a multi-select
// question), ←/→ change question, 0 writes another answer, Enter sends
// (approving a gate), x sends while rejecting a gate.
func (m Model) inlineKey(k tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	t, r := m.tasks.current(), m.tasks.pending
	if t == nil || r == nil || len(r.Questions) == 0 {
		return m, nil, false
	}
	a := m.answers(r)
	q := r.Questions[a.cur]
	key := k.String()
	switch {
	case len(key) == 1 && key >= "1" && key <= "9":
		n := int(key[0] - '1')
		if n >= len(q.Options) {
			return m, nil, true
		}
		if q.Multi {
			a.picks[a.cur][n] = !a.picks[a.cur][n]
		} else {
			a.picks[a.cur] = map[int]bool{n: true}
			a.others[a.cur] = ""
			// A lone question needs no confirmation (like a quick pick).
			if isQuestion(r) && len(r.Questions) == 1 {
				mm, cmd := m.sendInline(t, r, true)
				return mm, cmd, true
			}
			if a.cur < len(r.Questions)-1 {
				a.cur++
			}
		}
	case key == "right":
		a.cur = (a.cur + 1) % len(r.Questions)
	case key == "left":
		a.cur = (a.cur + len(r.Questions) - 1) % len(r.Questions)
	case key == "0":
		cur := a.cur
		f := newForm(fmt.Sprintf("Câu %d — câu trả lời khác", cur+1), func(v map[string]string) (tea.Cmd, error) {
			text := strings.TrimSpace(v["text"])
			return func() tea.Msg { return otherAnswerMsg{reqID: r.ID, q: cur, text: text} }, nil
		})
		f.hint = wrap(q.Question, 100)
		f.addArea("text", "Câu trả lời", a.others[cur], true)
		m.modal = f
		return m, f.start(), true
	case key == "enter" || key == "x":
		if key == "x" && !a.complete() {
			return m, nil, false // the usual reject form
		}
		if !a.complete() {
			m.msg = "còn câu chưa trả lời: chọn bằng phím số (←/→ đổi câu, 0 tự gõ)"
			return m, nil, true
		}
		mm, cmd := m.sendInline(t, r, key == "enter")
		return mm, cmd, true
	default:
		return m, nil, false
	}
	m.refreshTaskDetail(true)
	return m, nil, true
}

// otherAnswerMsg brings a "Khác" answer typed in its form.
type otherAnswerMsg struct {
	reqID string
	q     int
	text  string
}

func (m Model) onOtherAnswer(msg otherAnswerMsg) (tea.Model, tea.Cmd) {
	r := m.tasks.pending
	if r == nil || r.ID != msg.reqID {
		return m, nil
	}
	a := m.answers(r)
	a.others[msg.q] = msg.text
	if !r.Questions[msg.q].Multi {
		a.picks[msg.q] = map[int]bool{}
	}
	if a.cur == msg.q && a.cur < len(r.Questions)-1 {
		a.cur++
	}
	m.refreshTaskDetail(true)
	return m, nil
}

// sendInline decides the request with the inline answers.
func (m Model) sendInline(t *store.Task, r *store.ApprovalRequest, approve bool) (tea.Model, tea.Cmd) {
	a := m.answers(r)
	if err := m.d.Store.Decide(t.ID, r, approve, a.text(r.Questions)); err != nil {
		m.msg = "gửi câu trả lời: " + err.Error()
		return m, nil
	}
	verb := "đã trả lời " + r.ID
	if !isQuestion(r) {
		verb = map[bool]string{true: "đã duyệt " + r.ID + " kèm câu trả lời", false: "đã từ chối " + r.ID + " kèm câu trả lời"}[approve]
	}
	m.msg = verb
	m.tasks.answer = nil
	m.reloadTasks()
	return m, nil
}

// inlineQuestions renders the questions with the inline choices: ▸ marks the
// question the digits answer, ✓ the chosen options.
func inlineQuestions(r *store.ApprovalRequest, a *inlineAnswer, w int) string {
	var b strings.Builder
	for i, q := range r.Questions {
		mark := "  "
		if a != nil && i == a.cur && len(r.Questions) > 1 {
			mark = "▸ "
		}
		head := q.Question
		if len(r.Questions) > 1 {
			head = fmt.Sprintf("%d. %s", i+1, head)
		}
		if q.Header != "" {
			head = "[" + q.Header + "] " + head
		}
		if q.Multi {
			head += "  (chọn nhiều)"
		}
		b.WriteString(titleStyle.Render(wrap(mark+head, w)) + "\n")
		for j, o := range q.Options {
			chosen := a != nil && a.picks[i][j]
			box := "  "
			if chosen {
				box = "✓ "
			}
			line := fmt.Sprintf("  %s%d) %s", box, j+1, o.Label)
			if chosen {
				b.WriteString(selStyle.Render(line) + "\n")
			} else {
				b.WriteString(accentStyle.Render(wrap(line, w)) + "\n")
			}
			if o.Description != "" {
				b.WriteString(dimStyle.Render(wrap("       "+o.Description, w)) + "\n")
			}
		}
		other := "  0) Khác: tự trả lời"
		if a != nil && a.others[i] != "" {
			b.WriteString(selStyle.Render(truncate("  ✓ 0) Khác: "+a.others[i], w)) + "\n")
		} else {
			b.WriteString(dimStyle.Render(other) + "\n")
		}
	}
	return b.String()
}

// inlineKeys is the key hint of a pending request with questions.
func inlineKeys(r *store.ApprovalRequest) string {
	n := 0
	for _, q := range r.Questions {
		n = max(n, len(q.Options))
	}
	keys := fmt.Sprintf("1-%d chọn · 0 tự gõ", min(n, 9))
	if len(r.Questions) > 1 {
		keys += " · ←/→ đổi câu"
	}
	switch {
	case isQuestion(r) && len(r.Questions) == 1 && !r.Questions[0].Multi:
		// a pick sends at once
	case isQuestion(r):
		keys += " · Enter gửi"
	default:
		keys += " · Enter duyệt + gửi · x từ chối + gửi"
	}
	return keys + " · a form · m góp ý"
}
