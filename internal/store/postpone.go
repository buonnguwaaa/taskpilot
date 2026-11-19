package store

import (
	"database/sql"
	"errors"
	"time"
)

// Postponement is the operator putting a parked task on hold: it leaves the
// active group of the task list until resumed. The agent is not involved.
type Postponement struct {
	Note string
	At   time.Time
}

// Postpone puts a task on hold (again: the note and time are replaced).
func (s *Store) Postpone(id, note string) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO task_postponed (task_id, note, created_at) VALUES (?,?,?)
		ON CONFLICT (task_id) DO UPDATE SET note=excluded.note, created_at=excluded.created_at`,
		id, note, ts(time.Now()))
	return err
}

// Resume takes a task off hold; a task not on hold is left as it is.
func (s *Store) Resume(id string) error {
	_, err := s.DB.ExecContext(bg(), `DELETE FROM task_postponed WHERE task_id = ?`, id)
	return err
}

// Postponed returns the task's postponement, or nil.
func (s *Store) Postponed(id string) (*Postponement, error) {
	var p Postponement
	var at string
	err := s.DB.QueryRowContext(bg(), `SELECT note, created_at FROM task_postponed WHERE task_id = ?`, id).Scan(&p.Note, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.At = parseTS(at)
	return &p, nil
}

// PostponedIDs returns the ids of the tasks on hold.
func (s *Store) PostponedIDs() (map[string]bool, error) {
	rows, err := s.DB.QueryContext(bg(), `SELECT task_id FROM task_postponed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
