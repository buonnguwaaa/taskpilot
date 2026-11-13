package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// TaskWorker is the sticky headless worker for one task (claude or cursor),
// set when Claude hits quota and the run falls back to Cursor.
type TaskWorker struct {
	Agent  string    `json:"agent"`
	Reason string    `json:"reason,omitempty"`
	SetAt  time.Time `json:"set_at"`
}

func (s *Store) workerPath(id string) string {
	return filepath.Join(s.TaskDir(id), "worker.json")
}

// TaskWorker returns the sticky worker for a task, or nil.
func (s *Store) TaskWorker(id string) (*TaskWorker, error) {
	b, err := os.ReadFile(s.workerPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var w TaskWorker
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if w.Agent == "" {
		return nil, nil
	}
	return &w, nil
}

// SetTaskWorker pins the headless worker for the rest of the task.
func (s *Store) SetTaskWorker(id, agent, reason string) error {
	if err := os.MkdirAll(s.TaskDir(id), 0o755); err != nil {
		return err
	}
	w := TaskWorker{Agent: agent, Reason: reason, SetAt: time.Now().UTC()}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.workerPath(id), append(b, '\n'), 0o644)
}

// ClaudeQuotaLatch is a home-wide preference to skip Claude until Resets.
type ClaudeQuotaLatch struct {
	Reason string    `json:"reason"`
	Resets time.Time `json:"resets"`
	SetAt  time.Time `json:"set_at"`
}

func (s *Store) quotaLatchPath() string {
	return filepath.Join(s.Home, "claude-quota.json")
}

// ClaudeQuotaLatch returns the active latch, or nil when Claude may be tried.
func (s *Store) ClaudeQuotaLatch() (*ClaudeQuotaLatch, error) {
	b, err := os.ReadFile(s.quotaLatchPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var l ClaudeQuotaLatch
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	if l.Resets.IsZero() || !time.Now().Before(l.Resets) {
		_ = os.Remove(s.quotaLatchPath())
		return nil, nil
	}
	return &l, nil
}

// SetClaudeQuotaLatch records that Claude is exhausted until resets.
func (s *Store) SetClaudeQuotaLatch(resets time.Time, reason string) error {
	if resets.IsZero() {
		resets = time.Now().UTC().Add(time.Hour)
	}
	l := ClaudeQuotaLatch{Reason: reason, Resets: resets.UTC(), SetAt: time.Now().UTC()}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.quotaLatchPath(), append(b, '\n'), 0o644)
}

// ClearClaudeQuotaLatch drops the home-wide Claude skip.
func (s *Store) ClearClaudeQuotaLatch() error {
	err := os.Remove(s.quotaLatchPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
