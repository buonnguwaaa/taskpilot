package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// profileLine describes the model/effort the task's next run uses.
func (m Model) profileLine(t *store.Task) string {
	c := m.d.Config.Claude
	model, effort, who := c.Model, c.Effort, "mặc định config"
	p, _ := m.d.Store.TaskProfile(t.ID)
	if p != nil {
		if p.Model != "" {
			model = p.Model
		}
		if p.Effort != "" {
			effort = p.Effort
		}
		who = map[string]string{"agent": "agent chọn", "operator": "bạn ghim"}[p.SetBy]
	} else if a, _ := m.d.Store.TaskAssessment(t.ID); a != nil {
		if lp, ok := c.ForLevel(a.Level); ok {
			if lp.Model != "" {
				model = lp.Model
			}
			if lp.Effort != "" {
				effort = lp.Effort
			}
			who = "theo level " + levelLabels[a.Level]
		}
	}
	s := fmt.Sprintf("model %s · effort %s (%s · e đổi)", orDefaultVI(model), orDefaultVI(effort), who)
	// The agent often gives the same reason for the assessment, shown above.
	if a, _ := m.d.Store.TaskAssessment(t.ID); p != nil && p.Reason != "" && (a == nil || strings.TrimSpace(a.Reason) != strings.TrimSpace(p.Reason)) {
		s += "\n" + p.Reason
	}
	return s
}

func orDefaultVI(s string) string {
	if s == "" {
		return "mặc định"
	}
	return s
}

const agentPicks = "" // option value: let the agent choose

// editProfile opens the form that pins the task's model/effort, or hands the
// choice back to the agent.
func (m Model) editProfile(t *store.Task) (tea.Model, tea.Cmd) {
	if t == nil {
		return m, nil
	}
	cur, _ := m.d.Store.TaskProfile(t.ID)
	initModel, initEffort := agentPicks, agentPicks
	if cur != nil && cur.SetBy == "operator" {
		initModel, initEffort = cur.Model, cur.Effort
	}
	models := func(*form) []option {
		out := []option{{Label: "agent tự chọn", Value: agentPicks}}
		for _, x := range m.d.Config.Claude.Models {
			out = append(out, option{Label: x, Value: x})
		}
		return out
	}
	efforts := func(*form) []option {
		out := []option{{Label: "agent tự chọn", Value: agentPicks}}
		for _, x := range config.Efforts {
			out = append(out, option{Label: x, Value: x})
		}
		return out
	}
	st, id := m.d.Store, t.ID
	f := newForm("Model / effort — "+truncate(t.Title, 60), func(v map[string]string) (tea.Cmd, error) {
		if v["model"] == agentPicks && v["effort"] == agentPicks {
			if cur != nil && cur.SetBy == "operator" {
				return nil, st.ClearTaskProfile(id)
			}
			return nil, nil // already the agent's choice
		}
		return nil, st.SetTaskProfile(id, &store.Profile{Model: v["model"], Effort: v["effort"],
			Reason: "ghim bởi operator", SetBy: "operator"})
	})
	f.hint = "Áp dụng từ lượt chạy tiếp theo. Ghim thì agent không đổi được; chọn \"agent tự chọn\" ở cả hai để trả lại quyền cho agent." +
		"\nĐể trống một ô (agent tự chọn) khi ghim ô kia: ô trống dùng mặc định của config."
	if cur != nil && cur.SetBy == "agent" {
		f.hint += fmt.Sprintf("\nAgent đang chọn: %s / %s — %s", cur.Model, cur.Effort, cur.Reason)
	}
	f.addSelect("model", "Model", models, initModel)
	f.addSelect("effort", "Effort", efforts, initEffort)
	m.modal = f
	return m, f.start()
}
