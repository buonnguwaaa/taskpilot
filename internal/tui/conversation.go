package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
	"github.com/buonnguwaaa/taskpilot/internal/worklog"
)

// The task detail has two views (key t): the overview (state, gate, PRs,
// result) and the conversation: every run of the agent, what taskpilot sent
// it, what it wrote and did, what it asked and what you answered, in order.

// ---- assessment ----------------------------------------------------------------------

var levelLabels = map[string]string{"light": "Nhẹ", "standard": "Trung bình", "hard": "Khó", "hardest": "Rất khó"}

var levelColors = map[string]string{"light": cMuted, "standard": cSub, "hard": cText, "hardest": cErr}

var kindLabels = map[string]string{
	"feature": "Tính năng", "bugfix": "Sửa lỗi", "investigation": "Điều tra", "improvement": "Cải tiến",
	"refactor": "Refactor", "config": "Cấu hình", "test": "Test", "docs": "Tài liệu", "ops": "Vận hành", "other": "Khác",
}

// guessKind is the kind of a task the agent has not assessed yet, from how it
// was created ("" when nothing says).
func guessKind(t *store.Task) string {
	title := strings.ToLower(t.Title)
	switch {
	case strings.Contains(t.Body, splunk.InvestigationMarker), strings.Contains(t.Body, splunk.QCInvestigationMarker):
		return "investigation"
	case strings.HasPrefix(title, "sửa lỗi prod"), strings.HasPrefix(title, "sửa luồng lỗi"),
		strings.Contains(title, "[issue production]"), strings.Contains(title, "[bug]"), strings.Contains(title, "hotfix"):
		return "bugfix"
	case strings.Contains(title, "[cải tiến]"), strings.Contains(title, "[improve"):
		return "improvement"
	}
	return ""
}

// levelBadge is the colored level, e.g. "● Khó".
func levelBadge(level string) string {
	l, ok := levelLabels[level]
	if !ok {
		return dimStyle.Render("○ chưa đánh giá")
	}
	return fg(levelColors[level]).Render("● " + l)
}

// levelDot is the one-cell level mark of the task list.
func levelDot(a *store.Assessment) string {
	if a == nil || levelColors[a.Level] == "" {
		return " "
	}
	return lipgloss.NewStyle().Foreground(clr(levelColors[a.Level])).Render("●")
}

// assessmentLine is "Level ● Khó · Loại Sửa lỗi (agent: reason)".
func (m Model) assessmentLine(t *store.Task, w int) string {
	a, _ := m.d.Store.TaskAssessment(t.ID)
	level, kind, who := "", "", ""
	if a != nil {
		level, kind = a.Level, a.Kind
		who = "agent đánh giá"
		if a.SetBy == "estimate" {
			who = "Claude ước lượng từ ticket; agent sẽ đánh giá lại khi lập plan"
		}
	} else if m.tasks.assessing || m.d.Assess != nil && !m.tasks.assessTried[t.ID] {
		who = "Claude đang ước lượng…"
	}
	if kind == "" {
		if kind = guessKind(t); kind != "" {
			who = "đoán từ cách tạo task"
		}
	}
	kl := dimStyle.Render("chưa rõ")
	if l, ok := kindLabels[kind]; ok {
		kl = accentStyle.Render(l)
	}
	line := "Level " + levelBadge(level) + "   Loại " + kl
	if who != "" {
		line += dimStyle.Render("  (" + who + ")")
	} else if t.Runs <= 1 {
		line += dimStyle.Render("  (agent đánh giá khi lập plan)")
	}
	if a != nil && a.Reason != "" {
		line += "\n" + dimStyle.Render(wrap("↳ "+a.Reason, w))
	}
	return line
}

// ---- conversation --------------------------------------------------------------------

// convItem is one entry of a run's agent output.
type convItem struct {
	kind string // text, tool, toolerr, end
	text string
}

// convCache keeps the last event logs read, by path, size and time: the
// detail is rebuilt every second, and a long log takes long to parse.
var convCache = struct {
	sync.Mutex
	m map[string]convCached
}{m: map[string]convCached{}}

type convCached struct {
	size int64
	mod  time.Time
	segs [][]convItem
}

// convSegments reads the event log split per run (each run starts with a
// "system init" event).
func convSegments(path string) [][]convItem {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	convCache.Lock()
	c, ok := convCache.m[path]
	convCache.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c.segs
	}
	segs := readConvSegments(path)
	convCache.Lock()
	if len(convCache.m) >= 8 {
		convCache.m = map[string]convCached{}
	}
	convCache.m[path] = convCached{fi.Size(), fi.ModTime(), segs}
	convCache.Unlock()
	return segs
}

func readConvSegments(path string) [][]convItem {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const maxRead = 16 << 20
	cut := false
	if st, err := f.Stat(); err == nil && st.Size() > maxRead {
		_, _ = f.Seek(st.Size()-maxRead, io.SeekStart)
		cut = true
	}
	var segs [][]convItem
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	first := true
	for sc.Scan() {
		if first && cut { // a partial line
			first = false
			continue
		}
		first = false
		var ev rawEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Type == "system" && ev.Subtype == "init" {
			segs = append(segs, nil)
			continue
		}
		if len(segs) == 0 {
			segs = append(segs, nil)
		}
		cur := &segs[len(segs)-1]
		switch ev.Type {
		case "result":
			*cur = append(*cur, convItem{"end", fmt.Sprintf("kết thúc lượt (%s, $%.2f)", ev.Subtype, ev.TotalCostUSD)})
		case "assistant", "user":
			var blocks []contentBlock
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if s := strings.TrimSpace(b.Text); s != "" && ev.Type == "assistant" {
						*cur = append(*cur, convItem{"text", s})
					}
				case "tool_use":
					*cur = append(*cur, convItem{"tool", strings.TrimPrefix(b.Name, "mcp__taskpilot__") + " " + briefInput(b.Input)})
				case "tool_result":
					if b.IsError {
						*cur = append(*cur, convItem{"toolerr", firstLine(toolResultText(b.Content))})
					}
				}
			}
		}
	}
	return segs
}

// convEvent is something the operator or taskpilot did, placed by time.
type convEvent struct {
	at   time.Time
	text string
}

var triggerLabels = map[string]string{
	"initial": "bắt đầu", "approval": "sau khi bạn duyệt", "rejection": "sau khi bạn từ chối", "operator_message": "sau tin nhắn của bạn",
}

// maxToolRun is how many tool calls in a row are listed before folding.
const maxToolRun = 6

// conversation renders the task's whole conversation.
func (m Model) conversation(t *store.Task, w int) string {
	st := m.d.Store
	runs, _ := st.RunPrompts(t.ID)
	segs := convSegments(st.EventsPath(t.ID))
	// A log cut at the start: align the last segments with the last runs.
	if off := len(runs) - len(segs); off > 0 {
		segs = append(make([][]convItem, off), segs...)
	}
	var ops []convEvent
	reqs, _ := st.Requests(t.ID)
	decs, _ := st.Decisions(t.ID)
	for _, r := range reqs {
		head := "● Agent xin duyệt " + r.ID
		if r.Gate == store.QuestionGate {
			head = "● Agent hỏi bạn"
		}
		text := warnStyle.Render(head) + dimStyle.Render("  "+r.CreatedAt.Local().Format("02/01 15:04")) + "\n" + mdWrap(r.Summary, w)
		for _, q := range r.Questions {
			text += "\n" + accentStyle.Render(wrap("? "+q.Question, w))
			for _, o := range q.Options {
				text += "\n" + dimStyle.Render(wrap("   · "+o.Label, w))
			}
		}
		if d := strings.TrimSpace(r.Details); d != "" {
			text += "\n" + mdWrap(d, w)
		}
		ops = append(ops, convEvent{r.CreatedAt, text})
		if d, ok := decs[r.ID]; ok {
			verb := map[bool]string{true: "✓ Bạn duyệt " + r.ID, false: "✗ Bạn từ chối " + r.ID}[d.Approved]
			if r.Gate == store.QuestionGate {
				verb = "↩ Bạn trả lời"
			}
			dt := titleStyle.Render(verb) + dimStyle.Render("  "+d.DecidedAt.Local().Format("02/01 15:04"))
			if c := strings.TrimSpace(d.Comment); c != "" {
				dt += "\n" + wrap(c, w)
			}
			ops = append(ops, convEvent{d.DecidedAt, dt})
		}
	}
	msgs, _ := st.OperatorMessages(t.ID)
	for _, msg := range msgs {
		head := "→ Bạn nhắn agent"
		if !msg.Consumed {
			head += " (chưa gửi: agent nhận ở lượt chạy tới)"
		}
		ops = append(ops, convEvent{msg.At, titleStyle.Render(head) + dimStyle.Render("  "+msg.At.Local().Format("02/01 15:04")) + "\n" + wrap(msg.Text, w)})
	}
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].at.Before(ops[j].at) })

	var b strings.Builder
	b.WriteString(titleStyle.Render("Ticket") + "\n" + wrap(clipLines(t.Body, 25), w) + "\n")
	next := 0 // the next operator event to place
	flush := func(before time.Time) {
		for next < len(ops) && (before.IsZero() || ops[next].at.Before(before)) {
			b.WriteString("\n" + ops[next].text + "\n")
			next++
		}
	}
	n := max(len(runs), len(segs))
	for i := 0; i < n; i++ {
		var r *store.RunPrompt
		if i < len(runs) {
			r = runs[i]
			flush(r.StartedAt)
		}
		head := fmt.Sprintf("── Lượt %d", i+1)
		if r == nil {
			head = "── Phiên ngoài daemon (làm tay / takeover)"
		}
		if r != nil {
			head += " · " + triggerLabels[r.Trigger] + " · " + r.StartedAt.Local().Format("02/01 15:04")
			if r.Model != "" || r.Effort != "" {
				head += " · " + orDefaultVI(r.Model) + "/" + orDefaultVI(r.Effort)
			}
		}
		style := accentStyle
		if r != nil && r.Revoked {
			head += " · ↶ đã thu hồi (agent không còn thấy lượt này)"
			style = dimStyle
		}
		b.WriteString("\n" + style.Render(truncate(head+" "+strings.Repeat("─", w), w)) + "\n")
		if r != nil && r.Trigger == "operator_message" || r != nil && r.Trigger == "rejection" {
			// What the agent was told: your words are in the event above.
			b.WriteString(dimStyle.Render(wrap("taskpilot → agent: "+firstLine(r.Prompt), w)) + "\n")
		}
		if i < len(segs) {
			tools := 0
			for _, it := range segs[i] {
				if it.kind != "tool" {
					if tools > maxToolRun {
						b.WriteString(dimStyle.Render(fmt.Sprintf("   … và %d thao tác khác", tools-maxToolRun)) + "\n")
					}
					tools = 0
				}
				switch it.kind {
				case "text":
					b.WriteString(titleStyle.Render("Agent") + "\n" + mdWrap(it.text, w) + "\n")
				case "tool":
					tools++
					if tools <= maxToolRun {
						b.WriteString(dimStyle.Render(truncate("   → "+it.text, w)) + "\n")
					}
				case "toolerr":
					b.WriteString(errStyle.Render(truncate("   ✗ "+it.text, w)) + "\n")
				case "end":
					b.WriteString(dimStyle.Render("   ■ "+it.text) + "\n")
				}
			}
			if tools > maxToolRun {
				b.WriteString(dimStyle.Render(fmt.Sprintf("   … và %d thao tác khác", tools-maxToolRun)) + "\n")
			}
		}
		if t.State == store.StateRunning && i == n-1 {
			b.WriteString(accentStyle.Render("   ⋯ agent đang chạy") + "\n")
		}
	}
	flush(time.Time{})
	if c, _ := st.Completion(t.ID); c != nil {
		b.WriteString("\n" + titleStyle.Render("✓ Hoàn thành") + "\n" + mdWrap(c.Summary, w) + "\n")
	}
	if n == 0 && len(ops) == 0 {
		b.WriteString("\n" + dimStyle.Render("Agent chưa chạy lượt nào."))
	}
	return b.String()
}

// clipLines keeps the first n lines of s.
func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (%d dòng nữa)", len(lines)-n)
}

// assessedMsg is the end of a Claude estimate of a task's level and kind.
type assessedMsg struct {
	taskID string
	a      *worklog.Assessment
	err    error
}

// assessNext estimates, one task at a time, the tasks the agent has not
// assessed: the selected one first.
func (m Model) assessNext() (Model, tea.Cmd) {
	if m.d.Assess == nil || m.tasks.assessing {
		return m, nil
	}
	if m.tasks.assessTried == nil {
		m.tasks.assessTried = map[string]bool{}
	}
	var pick *store.Task
	cands := m.tasks.list
	if cur := m.tasks.current(); cur != nil {
		cands = append([]*store.Task{cur}, cands...)
	}
	for _, t := range cands {
		if m.tasks.assess[t.ID] == nil && !m.tasks.assessTried[t.ID] {
			pick = t
			break
		}
	}
	if pick == nil {
		return m, nil
	}
	m.tasks.assessing, m.tasks.assessTried[pick.ID] = true, true
	in := worklog.AssessInput{Language: m.d.Config.OperatorLanguage, Project: pick.Project, Title: pick.Title, Ticket: pick.Body,
		Completion: pick.LastResult}
	if c, _ := m.d.Store.Completion(pick.ID); c != nil {
		in.Completion = c.Summary
	}
	gen, id := m.d.Assess, pick.ID
	return m, func() tea.Msg {
		a, err := gen(context.Background(), in)
		return assessedMsg{taskID: id, a: a, err: err}
	}
}

func (m Model) onAssessed(msg assessedMsg) (tea.Model, tea.Cmd) {
	m.tasks.assessing = false
	if msg.err == nil && msg.a != nil {
		_ = m.d.Store.SetAssessment(msg.taskID, &store.Assessment{Level: msg.a.Level, Kind: msg.a.Kind, Reason: msg.a.Reason, SetBy: "estimate"})
	}
	m.reloadTasks()
	m.refreshTaskDetail(true)
	return m.assessNext()
}
