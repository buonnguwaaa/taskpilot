package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// stepState is where a task stands on one step of the workflow.
type stepState int

const (
	stepTodo     stepState = iota
	stepActive             // the agent works on it
	stepWaiting            // waiting for the operator (a gate request)
	stepRejected           // the operator sent it back; the agent revises
	stepStopped            // the task stopped here (needs attention, failed)
	stepAsked              // the agent asked the operator a question
	stepDone
	stepSkipped // the task finished without it (no QC, an investigation)
)

type step struct {
	label string
	state stepState
}

// workflowSteps places a task on the workflow of the workflow skill:
// plan gate → code → pr gate → pull requests → qc gate → done.
func workflowSteps(t *store.Task, reqs []*store.ApprovalRequest, decs map[string]*store.Decision, pending *store.ApprovalRequest, prs []*store.PullRequest) []step {
	gate := func(g string) stepState {
		st, asked := stepTodo, false
		for _, r := range reqs {
			if r.Gate != g {
				continue
			}
			asked = true
			if d := decs[r.ID]; d != nil && d.Approved {
				return stepDone
			} else if d != nil {
				st = stepRejected // the latest decision so far
			}
		}
		if pending != nil && pending.Gate == g {
			return stepWaiting
		}
		if !asked {
			return stepTodo
		}
		return st
	}
	asked := func(g string) bool {
		for _, r := range reqs {
			if r.Gate == g {
				return true
			}
		}
		return false
	}
	steps := []step{
		{"Plan", gate("plan")},
		{"Code", stepTodo},
		{"Review", gate("pr")},
		{"PR", stepTodo},
		{"QC", gate("qc")},
		{"Xong", stepTodo},
	}
	if asked("pr") {
		steps[1].state = stepDone
	}
	if len(prs) > 0 {
		steps[3].state = stepDone
	}
	if t.State == store.StateDone {
		steps[5].state = stepDone
	}
	if t.State == store.StateDone { // what did not happen was skipped (no QC, an investigation)
		for i := range steps {
			if steps[i].state != stepDone {
				steps[i].state = stepSkipped
			}
		}
		return steps
	}
	// Later steps reached mean the earlier ones are behind the task.
	last := -1
	for i, s := range steps {
		if s.state != stepTodo {
			last = i
		}
	}
	for i := 0; i < last; i++ {
		if steps[i].state == stepTodo {
			steps[i].state = stepDone
		}
	}
	// The current step: the first one not done. A rejected gate stays current
	// (↺) while the agent revises; a question waits for the operator.
	for i, s := range steps {
		if s.state == stepDone || s.state == stepSkipped || s.state == stepWaiting {
			if s.state == stepWaiting {
				break
			}
			continue
		}
		switch {
		case t.State == store.StateNeedsAttention || t.State == store.StateFailed:
			steps[i].state = stepStopped
		case pending != nil && pending.Gate == store.QuestionGate:
			steps[i].state = stepAsked
		case s.state == stepTodo:
			steps[i].state = stepActive
		}
		break
	}
	return steps
}

var stepLooks = map[stepState]struct {
	mark  string
	color string
}{
	stepTodo:     {"○", "8"},
	stepActive:   {"●", "14"},
	stepWaiting:  {"⏸", "11"},
	stepRejected: {"↺", "208"},
	stepStopped:  {"✗", "208"},
	stepAsked:    {"?", "11"},
	stepDone:     {"✓", "10"},
	stepSkipped:  {"–", "8"},
}

// workflowBar draws the progress bar and the steps of a task.
func workflowBar(steps []step, w int) string {
	done := 0
	for _, s := range steps {
		if s.state == stepDone || s.state == stepSkipped {
			done++
		}
	}
	var parts []string
	var now string
	for _, s := range steps {
		l := stepLooks[s.state]
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(l.color))
		if prefix, ok := map[stepState]string{stepActive: "agent đang làm: ", stepWaiting: "chờ bạn duyệt: ",
			stepStopped: "đang dừng ở: ", stepAsked: "chờ bạn trả lời câu hỏi: ", stepRejected: "agent đang sửa theo góp ý: "}[s.state]; ok {
			st = st.Bold(true)
			now = prefix + s.label
		}
		parts = append(parts, st.Render(l.mark+" "+s.label))
	}
	barW := min(30, max(10, w-20))
	fill := barW * done / len(steps)
	bar := accentStyle.Render(strings.Repeat("━", fill)) + dimStyle.Render(strings.Repeat("━", barW-fill))
	head := fmt.Sprintf("%s %s", bar, dimStyle.Render(fmt.Sprintf("%d/%d", done, len(steps))))
	if now != "" {
		head += "  " + dimStyle.Render(now)
	}
	return head + "\n" + strings.Join(parts, dimStyle.Render(" ─ "))
}
