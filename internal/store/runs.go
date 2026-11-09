package store

import (
	"database/sql"
	"errors"
	"time"
)

// RunStart describes a run about to begin.
type RunStart struct {
	N         int
	SessionID string
	Resume    bool
	Trigger   string // initial, approval, rejection, operator_message
	Prompt    string
	Model     string
	Effort    string
}

// RunEnd is what a finished run produced.
type RunEnd struct {
	Subtype          string
	IsError          bool
	Error            string
	NumTurns         int
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Outcome          string // awaiting_approval, done, needs_attention, failed, interrupted
}

// ToolCall is one tool use of a run (input already summarised and redacted).
type ToolCall struct {
	Seq          int
	ToolUseID    string
	Name         string
	InputSummary string
	IsError      bool
	DeniedBy     string // "", hook, permission
	DenyReason   string
}

// StartRun records a run and returns its id.
func (s *Store) StartRun(taskID string, r RunStart) (int64, error) {
	trigger := r.Trigger
	if trigger == "" {
		trigger = "initial"
	}
	res, err := s.DB.ExecContext(bg(), `INSERT INTO runs (task_id, n, session_id, resume, trigger, prompt, model, effort,
		started_at, events_path, stderr_path) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, r.N, r.SessionID, r.Resume, trigger, r.Prompt, r.Model, r.Effort, ts(time.Now()),
		s.EventsPath(taskID), s.StderrPath(taskID, r.N))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stores a run's result and its tool calls.
func (s *Store) FinishRun(runID int64, e RunEnd, calls []ToolCall) error {
	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var started string
	if err := tx.QueryRow(`SELECT started_at FROM runs WHERE id = ?`, runID).Scan(&started); err != nil {
		return err
	}
	var outcome any
	if e.Outcome != "" {
		outcome = e.Outcome
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE runs SET ended_at=?, duration_ms=?, result_subtype=?, is_error=?, error=?, num_turns=?,
		cost_usd=?, input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?, outcome=? WHERE id=?`,
		ts(now), now.Sub(parseTS(started)).Milliseconds(), e.Subtype, e.IsError, e.Error, e.NumTurns, e.CostUSD,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, outcome, runID); err != nil {
		return err
	}
	for _, c := range calls {
		var denied any
		if c.DeniedBy != "" {
			denied = c.DeniedBy
		}
		if _, err := tx.Exec(`INSERT INTO tool_calls (run_id, seq, tool_use_id, tool_name, input_summary, is_error, denied_by, deny_reason)
			VALUES (?,?,?,?,?,?,?,?)`, runID, c.Seq, c.ToolUseID, c.Name, c.InputSummary, c.IsError, denied, c.DenyReason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Run is a stored run (for reports).
type Run struct {
	ID        int64
	N         int
	Trigger   string
	StartedAt time.Time
	Duration  time.Duration
	CostUSD   float64
	NumTurns  int
	Outcome   string
	Subtype   string
	ToolCalls int
	Denied    int
}

// Runs lists a task's runs, oldest first.
func (s *Store) Runs(taskID string) ([]Run, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT r.id, r.n, r.trigger, r.started_at, coalesce(r.duration_ms,0), r.cost_usd, r.num_turns,
		coalesce(r.outcome,''), r.result_subtype,
		(SELECT count(*) FROM tool_calls c WHERE c.run_id = r.id),
		(SELECT count(*) FROM tool_calls c WHERE c.run_id = r.id AND c.denied_by IS NOT NULL)
		FROM runs r WHERE r.task_id = ? ORDER BY r.n`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started string
		var ms int64
		if err := rows.Scan(&r.ID, &r.N, &r.Trigger, &started, &ms, &r.CostUSD, &r.NumTurns, &r.Outcome, &r.Subtype, &r.ToolCalls, &r.Denied); err != nil {
			return nil, err
		}
		r.StartedAt, r.Duration = parseTS(started), time.Duration(ms)*time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- outbox ------------------------------------------------------------------------

// OutboxItem is a report waiting to reach the ticket system.
type OutboxItem struct {
	ID       int64
	TaskID   string
	Source   string
	TicketID string
	Message  string
	Attempts int
}

// Enqueue adds a report to the outbox and returns its id.
func (s *Store) Enqueue(taskID, source, ticketID, msg string) (int64, error) {
	now := ts(time.Now())
	res, err := s.DB.ExecContext(bg(), `INSERT INTO outbox (task_id, source, ticket_id, message, next_attempt_at, created_at) VALUES (?,?,?,?,?,?)`,
		taskID, source, ticketID, msg, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DueOutbox returns pending reports whose next attempt time has come, oldest first.
func (s *Store) DueOutbox(limit int) ([]OutboxItem, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT id, task_id, source, ticket_id, message, attempts FROM outbox
		WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY id LIMIT ?`, ts(time.Now()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxItem
	for rows.Next() {
		var o OutboxItem
		if err := rows.Scan(&o.ID, &o.TaskID, &o.Source, &o.TicketID, &o.Message, &o.Attempts); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkSent records a delivered report.
func (s *Store) MarkSent(id int64) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE outbox SET status='sent', attempts=attempts+1, sent_at=?, last_error='' WHERE id=?`, ts(time.Now()), id)
	return err
}

// MarkFailed records a failed delivery: retried with exponential backoff, given
// up (status failed) after maxAttempts.
func (s *Store) MarkFailed(id int64, cause error, maxAttempts int) error {
	var attempts int
	if err := s.DB.QueryRowContext(bg(), `SELECT attempts FROM outbox WHERE id=?`, id).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	status := "pending"
	if attempts >= maxAttempts {
		status = "failed"
	}
	backoff := time.Duration(1<<min(attempts, 10)) * 30 * time.Second
	_, err := s.DB.ExecContext(bg(), `UPDATE outbox SET status=?, attempts=?, last_error=?, next_attempt_at=? WHERE id=?`,
		status, attempts, cause.Error(), ts(time.Now().Add(backoff)), id)
	return err
}

// ---- stats -----------------------------------------------------------------------

// GateStat is one row of v_gate_stats.
type GateStat struct {
	Gate                                  string
	Requests, Approved, Rejected, Pending int
	ApprovalRate                          float64 // -1 when nothing was decided yet
	AvgWaitMinutes                        float64
}

// Stats summarises activity for the status page.
type Stats struct {
	CostToday, Cost7d float64
	Runs7d            int
	TasksDone7d       int
	Gates             []GateStat
	TopDenials        []string
	OutboxPending     int
	OutboxFailed      int
}

// Stats computes the status-page summary.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	today := time.Now().Format("2006-01-02")
	week := ts(time.Now().AddDate(0, 0, -7))
	q := s.DB.QueryRowContext
	if err := q(bg(), `SELECT coalesce(sum(cost_usd),0) FROM runs WHERE substr(started_at,1,10) = ?`, today).Scan(&st.CostToday); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT coalesce(sum(cost_usd),0), count(*) FROM runs WHERE started_at >= ?`, week).Scan(&st.Cost7d, &st.Runs7d); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT count(*) FROM tasks WHERE state = 'done' AND closed_at >= ?`, week).Scan(&st.TasksDone7d); err != nil {
		return nil, err
	}
	if err := q(bg(), `SELECT count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE status='failed') FROM outbox`).
		Scan(&st.OutboxPending, &st.OutboxFailed); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(bg(), `SELECT gate, requests, approved, rejected, pending, approval_rate, avg_wait_minutes FROM v_gate_stats ORDER BY gate`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var g GateStat
		var rate, wait sql.NullFloat64
		if err := rows.Scan(&g.Gate, &g.Requests, &g.Approved, &g.Rejected, &g.Pending, &rate, &wait); err != nil {
			rows.Close()
			return nil, err
		}
		g.ApprovalRate, g.AvgWaitMinutes = -1, wait.Float64
		if rate.Valid {
			g.ApprovalRate = rate.Float64
		}
		st.Gates = append(st.Gates, g)
	}
	rows.Close()
	rows, err = s.DB.QueryContext(bg(), `SELECT tool_name || ' (' || denied_by || ') ×' || n FROM v_denials LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		st.TopDenials = append(st.TopDenials, d)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return st, nil
}
