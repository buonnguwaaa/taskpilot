package mcpserver

import (
	"fmt"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/hook"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// request_permission: a command the agent needs is outside the allowlist.
// The operator answers like ask_operator: allow for this task, allow for the
// project, or not; an allowed rule is let through by the gate hook.

// RequestPermissionIn is the input of request_permission.
type RequestPermissionIn struct {
	Rule    string `json:"rule" jsonschema:"the narrowest Claude Code permission rule that covers what you need, e.g. Bash(curl *), Bash(jq *), Bash(docker compose ps*), WebFetch(domain:docs.example.com); never Bash or Bash(*)"`
	Command string `json:"command" jsonschema:"the exact command (or call) that was denied"`
	Reason  string `json:"reason" jsonschema:"why the task needs it and what you will do with it, in the operator's language"`
}

// RequestPermission asks the operator to allow a rule.
func (h *Handlers) RequestPermission(in RequestPermissionIn) (string, error) {
	rule := strings.TrimSpace(in.Rule)
	if !hook.ValidRule(rule) {
		return "", fmt.Errorf("rule %q is not a narrow permission rule: name the command, e.g. Bash(curl *) (never Bash or Bash(*))", rule)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return "", fmt.Errorf("reason is required")
	}
	if pending, err := h.Store.PendingRequest(h.TaskID); err != nil {
		return "", err
	} else if pending != nil {
		return "", fmt.Errorf("request %s is still waiting for the operator; end your turn now", pending.ID)
	}
	summary := "Agent xin quyền chạy `" + rule + "`: " + strings.TrimSpace(in.Reason)
	details := ""
	if c := strings.TrimSpace(in.Command); c != "" {
		details = "Lệnh bị chặn:\n```\n" + c + "\n```"
	}
	qs := []store.Question{{
		Question: "Cho agent chạy `" + rule + "`?",
		Header:   "Quyền",
		Options: []store.Option{
			{Label: store.PermAllowTask, Description: "chỉ task này; agent làm tiếp ngay"},
			{Label: store.PermAllowProject, Description: "lưu cho mọi task của project này"},
			{Label: store.PermDeny, Description: "agent tìm cách khác"},
		},
	}}
	r, err := h.Store.AddRequestWithQuestions(h.TaskID, store.QuestionGate, summary, details, qs)
	if err != nil {
		return "", err
	}
	if err := h.Store.SetPermissionRequest(r.ID, h.TaskID, rule); err != nil {
		return "", err
	}
	return fmt.Sprintf("Permission for %s asked as request %s. End your turn now with a one-line status; "+
		"the session resumes with the operator's answer, and an allowed rule works right away.", rule, r.ID), nil
}

// grantedView lists the rules the operator allowed during tasks.
func (h *Handlers) grantedView(taskID, project string) []string {
	gs, _ := h.Store.Grants(taskID, project)
	out := []string{}
	for _, g := range gs {
		out = append(out, g.Rule+" ("+g.Scope+")")
	}
	return out
}
