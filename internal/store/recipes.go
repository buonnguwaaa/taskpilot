package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RecipeProposal is a debugging recipe proposed by reflect, waiting for the
// operator (or decided).
type RecipeProposal struct {
	ID        int64
	Project   string
	Name      string
	YAML      string
	Reason    string
	Evidence  []string
	Replaces  string
	Status    string // new, accepted, rejected
	CreatedAt time.Time
}

// AddRecipeProposal records a proposal and returns its id.
func (s *Store) AddRecipeProposal(p *RecipeProposal) (int64, error) {
	res, err := s.DB.ExecContext(bg(), `INSERT INTO recipe_proposals (project, name, yaml, reason, evidence, replaces, created_at)
		VALUES (?,?,?,?,?,?,?)`, p.Project, p.Name, p.YAML, p.Reason, strings.Join(p.Evidence, "\n"), p.Replaces, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecipeProposals lists a project's proposals ("" = every project) with one
// of the statuses (none = all), newest first.
func (s *Store) RecipeProposals(project string, statuses ...string) ([]*RecipeProposal, error) {
	q := `SELECT id, project, name, yaml, reason, evidence, replaces, status, created_at FROM recipe_proposals WHERE (? = '' OR project = ?)`
	args := []any{project, project}
	if len(statuses) > 0 {
		q += ` AND status IN (?` + strings.Repeat(",?", len(statuses)-1) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	rows, err := s.DB.QueryContext(bg(), q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RecipeProposal
	for rows.Next() {
		var p RecipeProposal
		var ev, at string
		if err := rows.Scan(&p.ID, &p.Project, &p.Name, &p.YAML, &p.Reason, &ev, &p.Replaces, &p.Status, &at); err != nil {
			return nil, err
		}
		if ev != "" {
			p.Evidence = strings.Split(ev, "\n")
		}
		p.CreatedAt = parseTS(at)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// RecipeProposal returns one proposal.
func (s *Store) RecipeProposal(id int64) (*RecipeProposal, error) {
	var p RecipeProposal
	var ev, at string
	err := s.DB.QueryRowContext(bg(), `SELECT id, project, name, yaml, reason, evidence, replaces, status, created_at FROM recipe_proposals WHERE id = ?`, id).
		Scan(&p.ID, &p.Project, &p.Name, &p.YAML, &p.Reason, &ev, &p.Replaces, &p.Status, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ev != "" {
		p.Evidence = strings.Split(ev, "\n")
	}
	p.CreatedAt = parseTS(at)
	return &p, nil
}

// DecideRecipeProposal accepts or rejects a proposal.
func (s *Store) DecideRecipeProposal(id int64, status string) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE recipe_proposals SET status = ?, decided_at = ? WHERE id = ?`, status, ts(time.Now()), id)
	return err
}

// EditRecipeProposal replaces a waiting proposal's recipe (the operator edited it).
func (s *Store) EditRecipeProposal(id int64, name, yaml string) error {
	_, err := s.DB.ExecContext(bg(), `UPDATE recipe_proposals SET name = ?, yaml = ? WHERE id = ?`, name, yaml, id)
	return err
}

// ReflectRun is one reflection over a project's tasks.
type ReflectRun struct {
	Project   string
	At, Since time.Time
	Tasks     int
	Proposals int
	CostUSD   float64
	Error     string
}

// AddReflectRun records a reflection.
func (s *Store) AddReflectRun(r *ReflectRun) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO reflect_runs (project, at, since, tasks, proposals, cost_usd, error) VALUES (?,?,?,?,?,?,?)`,
		r.Project, ts(r.At), ts(r.Since), r.Tasks, r.Proposals, r.CostUSD, r.Error)
	return err
}

// LastReflect returns the project's latest reflection without error, or nil.
func (s *Store) LastReflect(project string) (*ReflectRun, error) {
	r := ReflectRun{Project: project}
	var at, since string
	err := s.DB.QueryRowContext(bg(), `SELECT at, since, tasks, proposals, cost_usd, error FROM reflect_runs
		WHERE project = ? AND error = '' ORDER BY id DESC LIMIT 1`, project).Scan(&at, &since, &r.Tasks, &r.Proposals, &r.CostUSD, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.At, r.Since = parseTS(at), parseTS(since)
	return &r, nil
}

// TaskNotes are what the operator wrote to a task's agent: messages, and
// comments on approval requests (with the decision).
func (s *Store) TaskNotes(id string) ([]string, error) {
	var out []string
	rows, err := s.DB.QueryContext(bg(), `SELECT message FROM operator_messages WHERE task_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, "nhắn: "+m)
	}
	rows.Close()
	rows, err = s.DB.QueryContext(bg(), `SELECT gate, coalesce(decision, ''), comment FROM approvals
		WHERE task_id = ? AND comment IS NOT NULL AND comment <> '' ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var gate, decision, c string
		if err := rows.Scan(&gate, &decision, &c); err != nil {
			return nil, err
		}
		out = append(out, "cổng "+gate+" "+decision+": "+c)
	}
	return out, rows.Err()
}
