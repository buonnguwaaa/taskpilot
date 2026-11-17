package store

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// Fold statistics: how tasks follow "When to fold subagents into your main
// agent loop" (the workflow skill's "Subagents: when to spawn, when not"):
// one main loop per task, a subagent only when it buys accuracy or keeps the
// context clean — explorers for wide reading, a reviewer on non-trivial
// diffs before the pr gate, implementers for disjoint parts — and never on
// Light tasks.

// FoldLevel is the tasks of one assessed level.
type FoldLevel struct {
	Level      string // light, standard, hard, hardest, "" (not assessed)
	Tasks      int
	WithSub    int     // tasks that spawned at least one subagent
	Subagents  int     // subagent calls
	Cost       float64 // average per task, USD
	Runs       float64 // average runs per task
	Rejections float64 // average rejected gate requests per task
}

// FoldGroup compares tasks that spawned subagents with tasks that did not.
type FoldGroup struct {
	Tasks      int
	Cost       float64 // average per task
	Runs       float64
	Rejections float64 // average rejected gate requests per task
	Decided    int     // decided gate requests
	Rejected   int
}

// FoldStats is the subagent usage of the tasks that ran since a time.
type FoldStats struct {
	Since  time.Time
	Tasks  int
	Folded int            // tasks done in the main loop only
	ByType map[string]int // subagent calls by type (explorer, reviewer, …)
	Levels []FoldLevel
	// LightSpawned are Light tasks that spawned subagents (the skill says not to).
	LightSpawned []string
	// NeedReview: Standard+ tasks that reached the pr gate; Reviewed: of
	// them, those that ran a reviewer before their first pr request.
	NeedReview, Reviewed int
	// Unreviewed lists the tasks of NeedReview without a reviewer.
	Unreviewed []string
	// With / Without: Standard+ tasks (where spawning is allowed).
	With, Without FoldGroup
}

var levelOrder = map[string]int{"light": 0, "standard": 1, "hard": 2, "hardest": 3, "": 4}

// subagentKind shortens "taskpilot:explorer" to "explorer".
func subagentKind(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.LastIndexByte(t, ':'); i >= 0 {
		t = t[i+1:]
	}
	if t == "" {
		return "general"
	}
	return t
}

// subagentTypes reads the type of each Agent call of a task from its event
// log (tool_use id → subagent_type), for the calls recorded before the
// summary carried it.
func (s *Store) subagentTypes(taskID string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(s.EventsPath(taskID))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"subagent_type"`) {
			continue
		}
		var ev struct {
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					ID    string `json:"id"`
					Name  string `json:"name"`
					Input struct {
						SubagentType string `json:"subagent_type"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		for _, b := range ev.Message.Content {
			if b.Type == "tool_use" && (b.Name == "Agent" || b.Name == "Task") && b.ID != "" {
				out[b.ID] = b.Input.SubagentType
			}
		}
	}
	return out
}

// FoldUsage computes the fold statistics of the tasks that ran since a time.
func (s *Store) FoldUsage(since time.Time) (*FoldStats, error) {
	fs := &FoldStats{Since: since, ByType: map[string]int{}}
	type task struct {
		id, level   string
		runs        int
		cost        float64
		decided     int
		rejected    int
		firstPR     time.Time
		subagents   int
		reviewedBy  time.Time // start of the first run with a reviewer
		hasReviewer bool
	}
	tasks := map[string]*task{}
	rows, err := s.DB.QueryContext(bg(), `SELECT r.task_id, coalesce(a.level, ''), count(*), coalesce(sum(r.cost_usd), 0)
		FROM runs r LEFT JOIN task_assessments a ON a.task_id = r.task_id
		WHERE r.task_id IN (SELECT task_id FROM runs WHERE started_at >= ?) GROUP BY r.task_id`, ts(since))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &task{}
		if err := rows.Scan(&t.id, &t.level, &t.runs, &t.cost); err != nil {
			rows.Close()
			return nil, err
		}
		tasks[t.id] = t
	}
	rows.Close()
	if len(tasks) == 0 {
		return fs, nil
	}
	rows, err = s.DB.QueryContext(bg(), `SELECT task_id, gate, created_at, coalesce(decision, '') FROM approvals`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, gate, created, decision string
		if err := rows.Scan(&id, &gate, &created, &decision); err != nil {
			rows.Close()
			return nil, err
		}
		t := tasks[id]
		if t == nil {
			continue
		}
		if decision != "" {
			t.decided++
		}
		if decision == "rejected" {
			t.rejected++
		}
		if at := parseTS(created); gate == "pr" && (t.firstPR.IsZero() || at.Before(t.firstPR)) {
			t.firstPR = at
		}
	}
	rows.Close()
	rows, err = s.DB.QueryContext(bg(), `SELECT r.task_id, r.started_at, c.tool_use_id, c.input_summary
		FROM tool_calls c JOIN runs r ON r.id = c.run_id WHERE c.tool_name IN ('Agent', 'Task')`)
	if err != nil {
		return nil, err
	}
	logTypes := map[string]map[string]string{}
	for rows.Next() {
		var id, started, useID, summary string
		if err := rows.Scan(&id, &started, &useID, &summary); err != nil {
			rows.Close()
			return nil, err
		}
		t := tasks[id]
		if t == nil {
			continue
		}
		// New calls carry "type description"; older ones are read from the log.
		typ, _, _ := strings.Cut(strings.TrimSpace(summary), " ")
		if !strings.Contains(typ, ":") && typ != "general-purpose" && typ != "Explore" && typ != "Plan" && typ != "web-fetch" {
			if logTypes[id] == nil {
				logTypes[id] = s.subagentTypes(id)
			}
			typ = logTypes[id][useID]
		}
		kind := subagentKind(typ)
		t.subagents++
		fs.ByType[kind]++
		if kind == "reviewer" {
			at := parseTS(started)
			if !t.hasReviewer || at.Before(t.reviewedBy) {
				t.reviewedBy, t.hasReviewer = at, true
			}
		}
	}
	rows.Close()

	levels := map[string]*FoldLevel{}
	add := func(g *FoldGroup, t *task) {
		g.Tasks++
		g.Cost += t.cost
		g.Runs += float64(t.runs)
		g.Rejections += float64(t.rejected)
		g.Decided += t.decided
		g.Rejected += t.rejected
	}
	for _, t := range tasks {
		fs.Tasks++
		if t.subagents == 0 {
			fs.Folded++
		}
		l := levels[t.level]
		if l == nil {
			l = &FoldLevel{Level: t.level}
			levels[t.level] = l
		}
		l.Tasks++
		l.Subagents += t.subagents
		l.Cost += t.cost
		l.Runs += float64(t.runs)
		l.Rejections += float64(t.rejected)
		if t.subagents > 0 {
			l.WithSub++
		}
		if t.level == "light" && t.subagents > 0 {
			fs.LightSpawned = append(fs.LightSpawned, t.id)
		}
		if t.level == "standard" || t.level == "hard" || t.level == "hardest" {
			if !t.firstPR.IsZero() {
				fs.NeedReview++
				if t.hasReviewer && !t.reviewedBy.After(t.firstPR) {
					fs.Reviewed++
				} else {
					fs.Unreviewed = append(fs.Unreviewed, t.id)
				}
			}
			if t.subagents > 0 {
				add(&fs.With, t)
			} else {
				add(&fs.Without, t)
			}
		}
	}
	for _, l := range levels {
		n := float64(l.Tasks)
		l.Cost, l.Runs, l.Rejections = l.Cost/n, l.Runs/n, l.Rejections/n
		fs.Levels = append(fs.Levels, *l)
	}
	sort.Slice(fs.Levels, func(i, j int) bool { return levelOrder[fs.Levels[i].Level] < levelOrder[fs.Levels[j].Level] })
	for _, g := range []*FoldGroup{&fs.With, &fs.Without} {
		if g.Tasks > 0 {
			n := float64(g.Tasks)
			g.Cost, g.Runs, g.Rejections = g.Cost/n, g.Runs/n, g.Rejections/n
		}
	}
	sort.Strings(fs.LightSpawned)
	sort.Strings(fs.Unreviewed)
	return fs, nil
}
