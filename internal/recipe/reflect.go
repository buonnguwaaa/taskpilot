package recipe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/oneshot"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// ReflectTimeout bounds one reflection.
const ReflectTimeout = 6 * time.Minute

// DefaultWindow is how far back the first reflection of a project reads.
const DefaultWindow = 30 * 24 * time.Hour

// Reflector proposes recipes from what the operator and the agents did in a
// project's recent tasks, with one claude turn that has no tools.
type Reflector struct {
	Config *config.Config
	Store  *store.Store
	// Run executes claude with args and stdin and returns stdout. Nil: exec Config.Claude.Bin.
	Run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// Outcome is the result of a reflection.
type Outcome struct {
	Since     time.Time
	Tasks     int // tasks read
	Proposals []*store.RecipeProposal
	Dropped   []string // proposals that failed validation, with why
	CostUSD   float64
}

// lookupTools are the agent tools a recipe can replay.
var lookupTools = map[string]string{
	"db_query": "sql", "splunk_search": "splunk", "qc_logs": "qc_logs", "qc_apps": "qc_apps",
	"qc_config_get": "vault", "qc_config_list": "vault_list", "qc_kafka_messages": "kafka", "qc_kafka_topics": "kafka_topics",
	"qc_kafka_consumer_groups": "kafka_groups", "run_recipe": "run_recipe",
}

// Reflect reads the project's tasks since the last reflection (at most
// DefaultWindow back, or since when given) and records the proposals.
func (r *Reflector) Reflect(ctx context.Context, project string, since time.Time) (*Outcome, error) {
	if _, ok := r.Config.Projects[project]; !ok {
		return nil, fmt.Errorf("không có project %q", project)
	}
	if since.IsZero() {
		since = time.Now().Add(-DefaultWindow)
		if last, _ := r.Store.LastReflect(project); last != nil && last.At.After(since) {
			since = last.At
		}
	}
	digest, n, err := r.Digest(project, since)
	if err != nil {
		return nil, err
	}
	out := &Outcome{Since: since, Tasks: n}
	run := &store.ReflectRun{Project: project, At: time.Now(), Since: since, Tasks: n}
	defer func() { _ = r.Store.AddReflectRun(run) }()
	if n == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ReflectTimeout)
	defer cancel()
	exec := r.Run
	if exec == nil {
		exec = func(ctx context.Context, args []string, stdin string) ([]byte, error) {
			return oneshot.Exec(ctx, r.Config.Claude.Bin, args, stdin, ReflectTimeout)
		}
	}
	c := r.Config.Claude
	raw, err := exec(ctx, oneshot.Args(reflectSchema, fmt.Sprintf(reflectSystem, r.Config.OperatorLanguage), c.Model, c.SettingSources), digest)
	if err != nil {
		run.Error = err.Error()
		return nil, err
	}
	var meta struct {
		Cost float64 `json:"total_cost_usd"`
	}
	_ = json.Unmarshal(raw, &meta)
	out.CostUSD, run.CostUSD = meta.Cost, meta.Cost
	ans, _, err := oneshot.Answer(raw)
	if err != nil {
		run.Error = err.Error()
		return nil, err
	}
	var res struct {
		Proposals []proposalJSON `json:"proposals"`
	}
	if err := json.Unmarshal(ans, &res); err != nil {
		run.Error = err.Error()
		return nil, fmt.Errorf("đọc đề xuất: %w", err)
	}
	known := func(p string) bool { _, ok := r.Config.Projects[p]; return ok }
	for _, pj := range res.Proposals {
		rc := pj.recipe(project)
		if err := Validate(rc, known); err != nil {
			out.Dropped = append(out.Dropped, rc.Name+": "+err.Error())
			continue
		}
		p := &store.RecipeProposal{Project: project, Name: rc.Name, YAML: Marshal(rc), Reason: pj.Reason,
			Evidence: pj.Evidence, Replaces: pj.Replaces, Status: "new", CreatedAt: time.Now()}
		id, err := r.Store.AddRecipeProposal(p)
		if err != nil {
			return out, err
		}
		p.ID = id
		out.Proposals = append(out.Proposals, p)
	}
	run.Proposals = len(out.Proposals)
	return out, nil
}

// Digest is what the model reads: the project's existing recipes and
// decided proposals, then each task since with the operator's words, the
// lookups its agent ran (with their inputs) and its conclusion. It returns
// the number of tasks included.
func (r *Reflector) Digest(project string, since time.Time) (string, int, error) {
	tasks, err := r.Store.ListTasks()
	if err != nil {
		return "", 0, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) })
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\nToday: %s\n\n", project, time.Now().Format("2006-01-02"))
	p := r.Config.Projects[project]
	if p.QCDB != nil || p.LogsProduction != nil || len(p.VaultPaths) > 0 || len(p.KafkaTopics) > 0 {
		b.WriteString("Lookups this project has: ")
		var has []string
		if p.QCDB != nil {
			has = append(has, "sql (QC MySQL)")
		}
		if !p.NoLogs {
			has = append(has, "splunk (production logs; the project filter is added automatically, never write it)")
		}
		has = append(has, "qc_logs (QC pod logs by Argo CD app)")
		if len(p.VaultPaths) > 0 {
			has = append(has, "vault (QC secrets under "+strings.Join(p.VaultPaths, ", ")+")")
		}
		if len(p.KafkaTopics) > 0 {
			has = append(has, "kafka (QC topics "+strings.Join(p.KafkaTopics, ", ")+")")
		}
		b.WriteString(strings.Join(has, "; ") + "\n\n")
	}
	rs, _ := List(Dir(r.Store.Home), project)
	b.WriteString("## Existing recipes (do not propose them again; propose an improvement only with replaces)\n")
	if len(rs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, rc := range rs {
		b.WriteString("```yaml\n" + Marshal(rc) + "```\n")
	}
	ps, _ := r.Store.RecipeProposals(project, "rejected", "new")
	b.WriteString("\n## Proposals already made (rejected by the operator, or still waiting): do not propose these again\n")
	if len(ps) == 0 {
		b.WriteString("(none)\n")
	}
	for _, pr := range ps {
		desc := ""
		if rc, err := Parse([]byte(pr.YAML)); err == nil {
			desc = oneLine(rc.Description)
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", pr.Name, pr.Status, desc)
	}
	b.WriteString("\n## Tasks since " + since.Local().Format("2006-01-02 15:04") + ", oldest first\n")
	const total = 160000
	n := 0
	for _, t := range tasks {
		if t.Project != project || t.CreatedAt.Before(since) {
			continue
		}
		calls := r.lookups(t.ID)
		if len(calls) == 0 {
			continue // no lookup: nothing a recipe could replay
		}
		notes, _ := r.Store.TaskNotes(t.ID)
		var tb strings.Builder
		fmt.Fprintf(&tb, "\n### Task %s (%s): %s\n", t.ID, t.CreatedAt.Local().Format("2006-01-02"), oneLine(t.Title))
		if body := oneshot.Clip(t.Body, 1200); body != "" {
			tb.WriteString("Request:\n" + body + "\n")
		}
		if len(notes) > 0 {
			tb.WriteString("Operator wrote:\n")
			for _, x := range notes {
				tb.WriteString("- " + oneshot.Clip(oneLine(x), 400) + "\n")
			}
		}
		if len(calls) > 0 {
			tb.WriteString("Lookups the agent ran, in order:\n")
			for _, c := range calls {
				tb.WriteString("- " + c + "\n")
			}
		}
		if c, _ := r.Store.Completion(t.ID); c != nil && c.Summary != "" {
			tb.WriteString("Conclusion:\n" + oneshot.Clip(c.Summary, 1200) + "\n")
		} else if t.LastResult != "" {
			tb.WriteString("Last result:\n" + oneshot.Clip(t.LastResult, 800) + "\n")
		}
		s := tb.String()
		if len(s) > 9000 {
			s = s[:9000] + "\n… (task cut)\n"
		}
		if b.Len()+len(s) > total {
			b.WriteString("\n(older tasks left out: the digest is full)\n")
			break
		}
		b.WriteString(s)
		n++
	}
	return b.String(), n, nil
}

// lookups lists the agent's lookup calls of a task from its events, one
// line each: the kind, the input and whether it failed.
func (r *Reflector) lookups(id string) []string {
	f, err := os.Open(r.Store.EventsPath(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	type call struct {
		id, line string
	}
	var calls []call
	failed := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e struct {
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					IsError   bool            `json:"is_error"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		for _, c := range e.Message.Content {
			switch c.Type {
			case "tool_use":
				name := c.Name[strings.LastIndex(c.Name, "__")+1:]
				name = strings.TrimPrefix(name, "_")
				kind, ok := lookupTools[name]
				if !ok {
					continue
				}
				calls = append(calls, call{c.ID, kind + " " + oneshot.Clip(oneLine(string(c.Input)), 700)})
			case "tool_result":
				if c.IsError {
					failed[c.ToolUseID] = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if failed[c.id] {
			c.line += "  → FAILED"
		}
		out = append(out, c.line)
	}
	if len(out) > 60 {
		out = append(out[:60], fmt.Sprintf("… %d more", len(out)-60))
	}
	return out
}

// proposalJSON is one proposal as the model writes it.
type proposalJSON struct {
	Name        string   `json:"name"`
	Replaces    string   `json:"replaces"`
	Reason      string   `json:"reason"`
	Evidence    []string `json:"evidence"`
	Description string   `json:"description"`
	Params      []Param  `json:"params"`
	Steps       []struct {
		Title        string `json:"title"`
		Kind         string `json:"kind"`
		Query        string `json:"query"`
		Earliest     string `json:"earliest"`
		App          string `json:"app"`
		Filter       string `json:"filter"`
		SinceMinutes int    `json:"since_minutes"`
		Path         string `json:"path"`
		Topic        string `json:"topic"`
		Since        string `json:"since"`
		Limit        int    `json:"limit"`
	} `json:"steps"`
	Guide string `json:"guide"`
}

func (p proposalJSON) recipe(project string) *Recipe {
	rc := &Recipe{Name: strings.TrimSpace(p.Name), Project: project, Description: strings.TrimSpace(p.Description),
		Params: p.Params, Guide: strings.TrimSpace(p.Guide)}
	for _, s := range p.Steps {
		st := Step{Title: s.Title}
		switch s.Kind {
		case "sql":
			st.SQL = &SQLStep{Query: s.Query}
		case "splunk":
			st.Splunk = &SplunkStep{Query: s.Query, Earliest: s.Earliest}
		case "qc_logs":
			st.QCLogs = &QCLogsStep{App: s.App, Filter: s.Filter, SinceMinutes: s.SinceMinutes}
		case "vault":
			st.Vault = &VaultStep{Path: s.Path}
		case "kafka":
			st.Kafka = &KafkaStep{Topic: s.Topic, Filter: s.Filter, Since: s.Since, Limit: s.Limit}
		}
		rc.Steps = append(rc.Steps, st)
	}
	return rc
}

const reflectSchema = `{
  "type": "object",
  "properties": {
    "proposals": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "replaces": {"type": "string"},
          "reason": {"type": "string"},
          "evidence": {"type": "array", "items": {"type": "string"}},
          "description": {"type": "string"},
          "params": {"type": "array", "items": {"type": "object",
            "properties": {"name": {"type": "string"}, "desc": {"type": "string"}, "example": {"type": "string"}},
            "required": ["name", "desc", "example"], "additionalProperties": false}},
          "steps": {"type": "array", "items": {"type": "object",
            "properties": {
              "title": {"type": "string"},
              "kind": {"type": "string", "enum": ["sql", "splunk", "qc_logs", "vault", "kafka"]},
              "query": {"type": "string"}, "earliest": {"type": "string"},
              "app": {"type": "string"}, "filter": {"type": "string"}, "since_minutes": {"type": "integer"},
              "path": {"type": "string"}, "topic": {"type": "string"}, "since": {"type": "string"}, "limit": {"type": "integer"}
            },
            "required": ["title", "kind", "query", "earliest", "app", "filter", "since_minutes", "path", "topic", "since", "limit"],
            "additionalProperties": false}},
          "guide": {"type": "string"}
        },
        "required": ["name", "replaces", "reason", "evidence", "description", "params", "steps", "guide"],
        "additionalProperties": false
      }
    }
  },
  "required": ["proposals"],
  "additionalProperties": false
}`

const reflectSystem = `You turn the debugging work an operator keeps repeating into reusable recipes.
You get the recent tasks of one project: what the operator asked and wrote, the read-only lookups the agent ran
(sql = QC MySQL, splunk = production logs, qc_logs = QC pod logs, vault = QC secrets, kafka = QC topics) and the
conclusion. You have no tools: answer with the JSON object only.

A recipe is a fixed list of lookups with parameters ({name} placeholders) that taskpilot runs in one go, plus a
guide on reading the results. Propose one only when it pays off:
- the same kind of question came back in at least two tasks (e.g. "check order X", "why did transaction Y fail"),
  or one task repeated the same lookups for several ids;
- its lookups are the ones that actually answered the question (skip the dead ends, the FAILED calls, and the
  one-off exploration), generalised: literal order codes, transaction codes, request ids, dates become params.
Few, sharp recipes beat many: usually 0 to 3. Proposing nothing is a fine answer when nothing repeats.

Rules for the fields:
- name: lowercase words joined by '-', what it does (tra-don-payment, kiem-tra-callback-vnpay).
- replaces: the name of an existing recipe this one improves, else "".
- description: one or two sentences: what question it answers and when to use it (an agent reads it to decide).
- params: name in snake_case (code, request_id, trans_code), desc, and an example value seen in the tasks. Values
  are plain ids or words: never put quotes around a placeholder inside SPL; in SQL put it inside single quotes.
- steps: 1 to 8. Fill only the fields of the step's kind and leave the others "" or 0:
  sql: query = one read-only SELECT on the tables the agent used, filtered by the params, only the useful columns,
       with LIMIT. splunk: query = SPL base search (no project/namespace filter: it is added), earliest like -1d or -7d.
  qc_logs: app = an Argo CD app the agent used, filter = usually the param. vault: path. kafka: topic, filter, since like 2h.
  title: a few words in %[1]s.
- guide: in %[1]s, short markdown: what each result means, the usual causes seen in the tasks and what to check next.
- reason: in %[1]s, one sentence: why this is worth keeping. evidence: in %[1]s, 2-5 items "task-id: what was repeated".`
