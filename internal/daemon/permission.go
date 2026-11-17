package daemon

import (
	"fmt"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// applyPermission records the operator's answer to a permission request
// (request_permission) and returns the prompt that resumes the agent.
func (d *Daemon) applyPermission(t *store.Task, rule string, dec *store.Decision) (string, error) {
	answer := dec.Comment
	scope, target, where := "", "", ""
	switch {
	case !dec.Approved || strings.Contains(answer, store.PermDeny):
	case strings.Contains(answer, store.PermAllowProject):
		scope, target, where = store.GrantProject, t.Project, "for every task of project "+t.Project
	case strings.Contains(answer, store.PermAllowTask):
		scope, target, where = store.GrantTask, t.ID, "for this task"
	}
	if scope == "" {
		return fmt.Sprintf("[taskpilot] The operator did NOT allow %s. Their answer:\n%s\n"+
			"Do not run it; find another way (an allowed command or a taskpilot tool), or explain in your summary what is blocked.",
			rule, answer), nil
	}
	if err := d.Store.AddGrant(scope, target, rule); err != nil {
		return "", err
	}
	return fmt.Sprintf("[taskpilot] The operator allowed %s %s. Run the command again now: it is allowed. Their answer:\n%s",
		rule, where, answer), nil
}
