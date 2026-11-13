package store

import (
	"database/sql"
	"errors"
	"time"
)

// Taking back what the operator sent. A message not received yet is simply
// deleted (RevokeMessage). Once a run started from it, the operator asks
// (RequestUndo) and the daemon stops the run if needed, takes the run back
// (TakeBackRun) and makes the next run resume the agent's session at the
// entry before it (SetResumeAt), so the agent never saw it.

// PendingMessage is the newest message the agent has not received yet, or nil.
func (s *Store) PendingMessage(id string) (*OperatorMessage, error) {
	var m OperatorMessage
	var at string
	err := s.DB.QueryRowContext(bg(), `SELECT message, created_at FROM operator_messages
		WHERE task_id = ? AND consumed_at IS NULL ORDER BY id DESC LIMIT 1`, id).Scan(&m.Text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.At = parseTS(at)
	return &m, nil
}

// RevokeMessage takes back the newest message the agent has not received
// and returns its text ("" when there is none).
func (s *Store) RevokeMessage(id string) (string, error) {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var rowID int64
	var text string
	err = tx.QueryRow(`SELECT id, message FROM operator_messages WHERE task_id = ? AND consumed_at IS NULL ORDER BY id DESC LIMIT 1`, id).Scan(&rowID, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM operator_messages WHERE id = ? AND consumed_at IS NULL`, rowID); err != nil {
		return "", err
	}
	return text, tx.Commit()
}

// RequestUndo asks the daemon to take back the task's latest run.
func (s *Store) RequestUndo(id string) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO task_undo (task_id, requested_at) VALUES (?, ?)
		ON CONFLICT (task_id) DO UPDATE SET requested_at = excluded.requested_at`, id, ts(time.Now()))
	return err
}

// UndoRequested reports whether a take-back waits for the daemon.
func (s *Store) UndoRequested(id string) bool {
	var n int
	_ = s.DB.QueryRowContext(bg(), `SELECT count(*) FROM task_undo WHERE task_id = ? AND requested_at IS NOT NULL`, id).Scan(&n)
	return n > 0
}

// UndoRequestedAt is when the pending take-back was asked (zero: none).
func (s *Store) UndoRequestedAt(id string) time.Time {
	var at sql.NullString
	_ = s.DB.QueryRowContext(bg(), `SELECT requested_at FROM task_undo WHERE task_id = ?`, id).Scan(&at)
	if !at.Valid {
		return time.Time{}
	}
	return parseTS(at.String)
}

// ClearUndoRequest marks the take-back done.
func (s *Store) ClearUndoRequest(id string) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE task_undo SET requested_at = NULL WHERE task_id = ?`, id)
	return err
}

// SetResumeAt makes the next run resume the session at entry ("" clears it).
func (s *Store) SetResumeAt(id, entry string) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO task_undo (task_id, resume_at) VALUES (?, ?)
		ON CONFLICT (task_id) DO UPDATE SET resume_at = excluded.resume_at`, id, entry)
	return err
}

// ResumeAt is the session entry the next run resumes at ("" = the end).
func (s *Store) ResumeAt(id string) string {
	var e string
	_ = s.DB.QueryRowContext(bg(), `SELECT resume_at FROM task_undo WHERE task_id = ?`, id).Scan(&e)
	return e
}

// RunTrigger is what the operator sent that started a run.
type RunTrigger struct {
	Messages []string  // operator messages it delivered
	Request  string    // the request whose decision it delivered
	Approved bool      // that decision
	Comment  string    // its comment / answer
	At       time.Time // when the operator sent it
}

// TriggerOf finds what the operator sent that started run r: the messages
// consumed, or the decision made, after the previous run started (prev) and
// before r started.
func (s *Store) TriggerOf(id string, prev time.Time, r *RunPrompt) (*RunTrigger, error) {
	in := func(t time.Time) bool { return t.After(prev) && !t.After(r.StartedAt) }
	tr := &RunTrigger{}
	rows, err := s.DB.QueryContext(bg(), `SELECT message, created_at, consumed_at FROM operator_messages
		WHERE task_id = ? AND consumed_at IS NOT NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m, created, consumed string
		if err := rows.Scan(&m, &created, &consumed); err != nil {
			rows.Close()
			return nil, err
		}
		if in(parseTS(consumed)) {
			tr.Messages = append(tr.Messages, m)
			tr.At = parseTS(created)
		}
	}
	rows.Close()
	reqs, err := s.Requests(id)
	if err != nil {
		return nil, err
	}
	decs, err := s.Decisions(id)
	if err != nil {
		return nil, err
	}
	for _, q := range reqs {
		if d, ok := decs[q.ID]; ok && in(d.DecidedAt) {
			tr.Request, tr.Approved, tr.Comment, tr.At = q.ID, d.Approved, d.Comment, d.DecidedAt
		}
	}
	return tr, rows.Err()
}

// TakeBackRun undoes run r on the store: the run is marked revoked, the
// messages or decision that started it are taken back (the request is
// pending again), and what the agent left during it (an unanswered request,
// a completion) is dropped.
func (s *Store) TakeBackRun(id string, prev time.Time, r *RunPrompt) (*RunTrigger, error) {
	tr, err := s.TriggerOf(id, prev, r)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := ts(time.Now())
	if _, err := tx.Exec(`UPDATE runs SET revoked_at = ? WHERE id = ?`, now, r.ID); err != nil {
		return nil, err
	}
	// Messages consumed for this run.
	rows, err := tx.Query(`SELECT id, consumed_at FROM operator_messages WHERE task_id = ? AND consumed_at IS NOT NULL`, id)
	if err != nil {
		return nil, err
	}
	var del []int64
	for rows.Next() {
		var mid int64
		var at string
		if err := rows.Scan(&mid, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if t := parseTS(at); t.After(prev) && !t.After(r.StartedAt) {
			del = append(del, mid)
		}
	}
	rows.Close()
	for _, mid := range del {
		if _, err := tx.Exec(`DELETE FROM operator_messages WHERE id = ?`, mid); err != nil {
			return nil, err
		}
	}
	// Requests the agent raised during the run and nobody answered.
	rows, err = tx.Query(`SELECT id, created_at FROM approvals WHERE task_id = ? AND decision IS NULL`, id)
	if err != nil {
		return nil, err
	}
	var drop []string
	for rows.Next() {
		var rid, at string
		if err := rows.Scan(&rid, &at); err != nil {
			rows.Close()
			return nil, err
		}
		if !parseTS(at).Before(r.StartedAt) {
			drop = append(drop, rid)
		}
	}
	rows.Close()
	for _, rid := range drop {
		if _, err := tx.Exec(`DELETE FROM approvals WHERE task_id = ? AND id = ?`, id, rid); err != nil {
			return nil, err
		}
	}
	if tr.Request != "" {
		if _, err := tx.Exec(`UPDATE approvals SET decision = NULL, comment = '', decided_by = '', decided_at = NULL
			WHERE task_id = ? AND id = ?`, id, tr.Request); err != nil {
			return nil, err
		}
	}
	var cid int64
	var cat string
	err = tx.QueryRow(`SELECT id, created_at FROM completions WHERE task_id = ? AND archived_at IS NULL`, id).Scan(&cid, &cat)
	switch {
	case err == nil && !parseTS(cat).Before(r.StartedAt):
		if _, err := tx.Exec(`UPDATE completions SET archived_at = ? WHERE id = ?`, now, cid); err != nil {
			return nil, err
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	return tr, tx.Commit()
}
