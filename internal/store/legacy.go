package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// legacyFiles are the per-task files of the file-based store (before SQLite).
// events.jsonl and runs/ are not among them: they stay where they are.
var legacyFiles = []string{"task.json", "requests", "decisions", "completion.json", "nudge.json",
	"baselines", "prs", "splunk-queries.jsonl", "takeover"}

// ImportLegacy moves tasks of the old file-based store into the database.
// Each task is imported in one transaction, keeping its original timestamps,
// then its legacy files are moved to <task>/legacy/. Already-imported tasks
// (present in the database) are skipped. It returns the ids imported.
func (s *Store) ImportLegacy() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Home, "tasks"))
	if err != nil {
		return nil, err
	}
	var imported []string
	for _, e := range entries {
		dir := filepath.Join(s.Home, "tasks", e.Name())
		if !e.IsDir() || !exists(filepath.Join(dir, "task.json")) {
			continue
		}
		if s.Exists(e.Name()) {
			continue
		}
		if err := s.importTask(dir); err != nil {
			return imported, fmt.Errorf("import %s: %w", e.Name(), err)
		}
		if err := moveLegacy(dir); err != nil {
			return imported, fmt.Errorf("import %s: move legacy files: %w", e.Name(), err)
		}
		imported = append(imported, e.Name())
	}
	return imported, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func readJSONFile(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readJSONDir[T any](dir string) ([]T, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []T
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		var v T
		if err := readJSONFile(filepath.Join(dir, e.Name()), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) importTask(dir string) error {
	var t struct {
		Task
		Repo     string `json:"repo"`     // pre-project tasks
		Worktree string `json:"worktree"` // pre-workspace tasks
	}
	if err := readJSONFile(filepath.Join(dir, "task.json"), &t); err != nil {
		return err
	}
	task := t.Task
	if task.Project == "" {
		task.Project = t.Repo
	}
	if task.Workspace == "" {
		task.Workspace = t.Worktree
	}
	if !ValidID(task.ID) || task.ID != filepath.Base(dir) {
		return fmt.Errorf("task id %q does not match its directory", task.ID)
	}
	reqs, err := readJSONDir[ApprovalRequest](filepath.Join(dir, "requests"))
	if err != nil {
		return err
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].CreatedAt.Before(reqs[j].CreatedAt) })
	decs, err := readJSONDir[Decision](filepath.Join(dir, "decisions"))
	if err != nil {
		return err
	}
	decByReq := map[string]Decision{}
	for _, d := range decs {
		decByReq[d.RequestID] = d
	}
	baselines, err := readJSONDir[Baseline](filepath.Join(dir, "baselines"))
	if err != nil {
		return err
	}
	prs, err := readJSONDir[PullRequest](filepath.Join(dir, "prs"))
	if err != nil {
		return err
	}

	tx, err := s.DB.BeginTx(bg(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var closed any
	if task.State.Terminal() {
		closed = ts(task.UpdatedAt)
	}
	if _, err := tx.Exec(`INSERT INTO tasks (id, source, ticket_id, ticket_url, title, body, project, repo_hint, branch, workspace,
		session_id, state, next_prompt, next_trigger, runs_count, cost_usd, last_result, last_error, created_at, updated_at, closed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.Source, task.TicketID, task.TicketURL, task.Title, task.Body, task.Project, task.RepoHint, task.Branch,
		task.Workspace, task.SessionID, string(task.State), task.NextPrompt, task.NextTrigger, task.Runs, task.CostUSD,
		task.LastResult, task.LastError, ts(task.CreatedAt), ts(task.UpdatedAt), closed); err != nil {
		return err
	}
	for _, h := range task.History {
		if _, err := tx.Exec(`INSERT INTO task_transitions (task_id, at, state, note) VALUES (?,?,?,?)`,
			task.ID, ts(h.At), string(h.State), h.Note); err != nil {
			return err
		}
	}
	attempts := map[string]int{}
	for _, r := range reqs {
		attempts[r.Gate]++
		var decision, decidedAt any
		comment := ""
		if d, ok := decByReq[r.ID]; ok {
			decision = map[bool]string{true: "approved", false: "rejected"}[d.Approved]
			decidedAt, comment = ts(d.DecidedAt), d.Comment
		}
		if _, err := tx.Exec(`INSERT INTO approvals (task_id, id, gate, attempt, summary, details, created_at, decision, comment, decided_by, decided_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, task.ID, r.ID, r.Gate, attempts[r.Gate], r.Summary, r.Details, ts(r.CreatedAt),
			decision, comment, map[bool]string{true: "operator", false: ""}[decision != nil], decidedAt); err != nil {
			return err
		}
	}
	var c Completion
	if err := readJSONFile(filepath.Join(dir, "completion.json"), &c); err == nil {
		if _, err := tx.Exec(`INSERT INTO completions (task_id, summary, pr_url, created_at) VALUES (?,?,?,?)`,
			task.ID, c.Summary, c.PRURL, ts(c.CreatedAt)); err != nil {
			return err
		}
	}
	var n Nudge
	if err := readJSONFile(filepath.Join(dir, "nudge.json"), &n); err == nil && n.Message != "" {
		if _, err := tx.Exec(`INSERT INTO operator_messages (task_id, message, created_at) VALUES (?,?,?)`,
			task.ID, n.Message, ts(n.CreatedAt)); err != nil {
			return err
		}
	}
	for _, b := range baselines {
		if _, err := tx.Exec(`INSERT INTO baselines (task_id, repo, command, passed, output, duration_ms, at) VALUES (?,?,?,?,?,?,?)`,
			task.ID, b.Repo, b.Command, b.Passed, b.Output, b.DurationMS, ts(b.At)); err != nil {
			return err
		}
	}
	for _, p := range prs {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pull_requests (task_id, repo, destination, pr_id, url, source, title, description, pending, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, task.ID, p.Repo, p.Destination, p.ID, p.URL, p.Source, p.Title, p.Description, p.Pending,
			ts(p.CreatedAt)); err != nil {
			return err
		}
	}
	if f, err := os.Open(filepath.Join(dir, "splunk-queries.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var q SplunkQuery
			if json.Unmarshal(sc.Bytes(), &q) != nil {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO splunk_queries (task_id, at, query, earliest, latest, events, error) VALUES (?,?,?,?,?,?,?)`,
				task.ID, ts(q.At), q.Query, q.Earliest, q.Latest, q.Events, q.Error); err != nil {
				f.Close()
				return err
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile(filepath.Join(dir, "takeover")); err == nil {
		var pid int
		if _, err := fmt.Sscanf(string(b), "pid=%d", &pid); err == nil && pid > 0 {
			if _, err := tx.Exec(`INSERT INTO takeovers (task_id, pid, started_at) VALUES (?,?,?)`, task.ID, pid, ts(time.Now())); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// moveLegacy moves the imported files into <dir>/legacy/ (kept for reference).
func moveLegacy(dir string) error {
	dst := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, name := range legacyFiles {
		src := filepath.Join(dir, name)
		if !exists(src) {
			continue
		}
		if err := os.Rename(src, filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}
