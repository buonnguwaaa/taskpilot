package store

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// Token usage over time: task runs (runs) and the one-turn helper calls
// (claude_calls), for the Usage tab.

// ClaudeCall is the usage of one helper call (not a task run).
type ClaudeCall struct {
	At         time.Time
	Purpose    string
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
}

// AddClaudeCall records a helper call's usage.
func (s *Store) AddClaudeCall(c ClaudeCall) error {
	_, err := s.DB.ExecContext(bg(), `INSERT INTO claude_calls (at, purpose, model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd) VALUES (?,?,?,?,?,?,?,?)`,
		ts(c.At), c.Purpose, c.Model, c.Input, c.Output, c.CacheRead, c.CacheWrite, c.CostUSD)
	return err
}

// Tokens is a sum of usage.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
	Calls      int
}

// Total is every token: new input, output and both cache kinds.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

func (t *Tokens) add(u UsageRow) {
	t.Input += u.Input
	t.Output += u.Output
	t.CacheRead += u.CacheRead
	t.CacheWrite += u.CacheWrite
	t.CostUSD += u.CostUSD
	t.Calls++
}

// UsageRow is one task run or one helper call.
type UsageRow struct {
	At      time.Time
	Run     bool   // a task run; false: a helper call
	Source  string // the helper call's purpose ("" for a run)
	TaskID  string
	Title   string
	Project string
	Model   string
	Tokens
}

// UsageRows returns the runs and helper calls started in [from, to).
func (s *Store) UsageRows(from, to time.Time) ([]UsageRow, error) {
	var out []UsageRow
	rows, err := s.DB.QueryContext(bg(), `SELECT r.started_at, r.task_id, coalesce(t.title,''), coalesce(t.project,''), r.model,
		r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.cost_usd
		FROM runs r LEFT JOIN tasks t ON t.id = r.task_id WHERE r.started_at >= ? AND r.started_at < ?`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		u := UsageRow{Run: true}
		var at string
		if err := rows.Scan(&at, &u.TaskID, &u.Title, &u.Project, &u.Model,
			&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			rows.Close()
			return nil, err
		}
		u.At = parseTS(at)
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.DB.QueryContext(bg(), `SELECT at, purpose, model, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, cost_usd FROM claude_calls WHERE at >= ? AND at < ?`, ts(from), ts(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u UsageRow
		var at string
		if err := rows.Scan(&at, &u.Source, &u.Model, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			return nil, err
		}
		u.At = parseTS(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// FirstUsage is when the oldest run or helper call started (zero: none).
func (s *Store) FirstUsage() time.Time {
	var a, b sql.NullString
	s.DB.QueryRowContext(bg(), `SELECT min(started_at) FROM runs`).Scan(&a)
	s.DB.QueryRowContext(bg(), `SELECT min(at) FROM claude_calls`).Scan(&b)
	var first time.Time
	for _, v := range []sql.NullString{a, b} {
		if t := parseTS(v.String); v.Valid && !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first
}

// Bucket is how a period is cut.
type Bucket int

const (
	ByHour Bucket = iota
	ByDay
	ByWeek
	ByMonth
)

// Start is the bucket holding t (local time; weeks start on Monday).
func (b Bucket) Start(t time.Time) time.Time {
	t = t.Local()
	y, mo, d := t.Date()
	switch b {
	case ByHour:
		return time.Date(y, mo, d, t.Hour(), 0, 0, 0, time.Local)
	case ByWeek:
		day := time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case ByMonth:
		return time.Date(y, mo, 1, 0, 0, 0, 0, time.Local)
	}
	return time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
}

// Next is the bucket after the one starting at t.
func (b Bucket) Next(t time.Time) time.Time {
	switch b {
	case ByHour:
		return t.Add(time.Hour)
	case ByWeek:
		return t.AddDate(0, 0, 7)
	case ByMonth:
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(0, 0, 1)
}

// Named is a sum under a name (a model, a project, a source, a task).
type Named struct {
	Name   string
	Detail string // a task's title
	Tokens
}

// UsageSummary is the usage of a period, cut into buckets and broken down.
type UsageSummary struct {
	From, To time.Time
	Bucket   Bucket
	Total    Tokens
	Runs     Tokens // task runs only
	Helpers  Tokens // helper calls only
	Buckets  []Named
	Models   []Named
	Projects []Named
	Sources  []Named
	Tasks    []Named // the costliest tasks first
}

// SummarizeUsage sums rows over [from, to) cut by b.
func SummarizeUsage(rows []UsageRow, from, to time.Time, b Bucket) UsageSummary {
	s := UsageSummary{From: from, To: to, Bucket: b}
	idx := map[time.Time]int{}
	for t := b.Start(from); t.Before(to); t = b.Next(t) {
		idx[t] = len(s.Buckets)
		s.Buckets = append(s.Buckets, Named{Name: t.Format(time.RFC3339)})
	}
	models, projects, sources, tasks := map[string]*Named{}, map[string]*Named{}, map[string]*Named{}, map[string]*Named{}
	put := func(m map[string]*Named, key, detail string, u UsageRow) {
		n := m[key]
		if n == nil {
			n = &Named{Name: key, Detail: detail}
			m[key] = n
		}
		n.add(u)
	}
	for _, u := range rows {
		s.Total.add(u)
		if i, ok := idx[b.Start(u.At)]; ok {
			s.Buckets[i].add(u)
		}
		model := modelFamily(u.Model)
		put(models, model, "", u)
		if u.Run {
			s.Runs.add(u)
			project := u.Project
			if project == "" {
				project = "(không project)"
			}
			put(projects, project, "", u)
			put(sources, "task", "", u)
			put(tasks, u.TaskID, u.Title, u)
		} else {
			s.Helpers.add(u)
			source := u.Source
			if source == "" {
				source = "khác"
			}
			put(sources, source, "", u)
		}
	}
	s.Models, s.Projects, s.Sources, s.Tasks = ranked(models), ranked(projects), ranked(sources), ranked(tasks)
	return s
}

// ranked lists the sums, the most tokens first.
func ranked(m map[string]*Named) []Named {
	out := make([]Named, 0, len(m))
	for _, n := range m {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total() != out[j].Total() {
			return out[i].Total() > out[j].Total()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// modelFamily names a model the way task runs do (opus, sonnet, haiku): the
// helper calls report the full id.
func modelFamily(m string) string {
	if m == "" {
		return "mặc định"
	}
	for _, f := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(strings.ToLower(m), f) {
			return f
		}
	}
	return m
}
