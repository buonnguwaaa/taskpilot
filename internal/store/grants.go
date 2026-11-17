package store

import (
	"database/sql"
	"errors"
	"time"
)

// Tool grants: permission rules the operator added while tasks ran, on top
// of the project's allowlist (request_permission → the operator's answer).

// Grant scopes.
const (
	GrantTask    = "task"
	GrantProject = "project"
)

// Grant is one allowed rule.
type Grant struct {
	Scope  string // GrantTask or GrantProject
	Target string // task id or project name
	Rule   string
	At     time.Time
}

// AddGrant allows a rule for a task or a project.
func (s *Store) AddGrant(scope, target, rule string) error {
	if scope != GrantTask && scope != GrantProject {
		return errors.New("grant scope must be task or project")
	}
	_, err := s.DB.ExecContext(bg(), `INSERT INTO tool_grants (scope, target, rule, created_at) VALUES (?,?,?,?)
		ON CONFLICT DO NOTHING`, scope, target, rule, ts(time.Now()))
	return err
}

// RemoveGrant takes a rule back.
func (s *Store) RemoveGrant(scope, target, rule string) error {
	_, err := s.DB.ExecContext(bg(), `DELETE FROM tool_grants WHERE scope = ? AND target = ? AND rule = ?`, scope, target, rule)
	return err
}

// Grants lists the rules that apply to a task: its own and its project's.
func (s *Store) Grants(taskID, project string) ([]Grant, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT scope, target, rule, created_at FROM tool_grants
		WHERE (scope = 'task' AND target = ?) OR (scope = 'project' AND target = ?) ORDER BY created_at`, taskID, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var at string
		if err := rows.Scan(&g.Scope, &g.Target, &g.Rule, &at); err != nil {
			return nil, err
		}
		g.At = parseTS(at)
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetPermissionRequest records the rule a request asks for.
func (s *Store) SetPermissionRequest(requestID, taskID, rule string) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO permission_requests (request_id, task_id, rule) VALUES (?,?,?)`, requestID, taskID, rule)
	return err
}

// PermissionRule returns the rule a request asks for, or "" when the request
// is not a permission request.
func (s *Store) PermissionRule(taskID, requestID string) (string, error) {
	var rule string
	err := s.DB.QueryRowContext(bg(), `SELECT rule FROM permission_requests WHERE task_id = ? AND request_id = ?`, taskID, requestID).Scan(&rule)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return rule, err
}

// Answers of a permission request (the operator picks one; the daemon reads it).
const (
	PermAllowTask    = "Cho phép trong task này"
	PermAllowProject = "Luôn cho phép (mọi task của project)"
	PermDeny         = "Không cho phép"
)

// AllGrants lists every grant, newest first.
func (s *Store) AllGrants() ([]Grant, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT scope, target, rule, created_at FROM tool_grants ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var at string
		if err := rows.Scan(&g.Scope, &g.Target, &g.Rule, &at); err != nil {
			return nil, err
		}
		g.At = parseTS(at)
		out = append(out, g)
	}
	return out, rows.Err()
}
