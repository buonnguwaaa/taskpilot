package store

import (
	"database/sql"
	"errors"
	"time"
)

// TaskTicket is a ticket attached to a task besides its own (a bug of the
// feature the task builds): the same agent session works on both.
type TaskTicket struct {
	TaskID   string
	Source   string
	TicketID string
	Title    string
	At       time.Time
}

// AttachTicket records that a ticket is worked in a task. A ticket belongs
// to one task: attaching it again moves it.
func (s *Store) AttachTicket(t *TaskTicket) error {
	if t.At.IsZero() {
		t.At = time.Now()
	}
	_, err := s.DB.ExecContext(bg(), `INSERT INTO task_tickets (task_id, source, ticket_id, title, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT (source, ticket_id) DO UPDATE SET task_id=excluded.task_id, title=excluded.title, created_at=excluded.created_at`,
		t.TaskID, t.Source, t.TicketID, t.Title, ts(t.At))
	return err
}

// TicketHost returns the task a ticket is attached to, or "".
func (s *Store) TicketHost(source, ticketID string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(bg(), `SELECT task_id FROM task_tickets WHERE source = ? AND ticket_id = ?`, source, ticketID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// TaskTickets lists the tickets attached to a task, oldest first.
func (s *Store) TaskTickets(taskID string) ([]*TaskTicket, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT source, ticket_id, title, created_at FROM task_tickets WHERE task_id = ? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TaskTicket
	for rows.Next() {
		t := &TaskTicket{TaskID: taskID}
		var at string
		if err := rows.Scan(&t.Source, &t.TicketID, &t.Title, &at); err != nil {
			return nil, err
		}
		t.At = parseTS(at)
		out = append(out, t)
	}
	return out, rows.Err()
}
