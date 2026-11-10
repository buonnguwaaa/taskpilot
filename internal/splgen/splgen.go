// Package splgen turns an operator's question about production logs ("why
// did TCB refunds fail this morning?") into a read-only Splunk search, with
// one short claude turn that has no tools: it reads the project's log
// knowledge (namespaces, containers, fields, known searches) and answers with
// JSON. The search is validated before the operator sees it, and the
// project's filter is added when it runs, never by the model.
package splgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// Timeout bounds one generation.
const Timeout = 2 * time.Minute

// Result is a generated search.
type Result struct {
	SPL         string `json:"spl"`         // base search, without the project filter
	Earliest    string `json:"earliest"`    // Splunk relative time, e.g. -4h, @d, -1d@d
	Latest      string `json:"latest"`      // "" = now
	Explanation string `json:"explanation"` // one or two lines for the operator
}

const schema = `{
  "type": "object",
  "properties": {
    "spl": {"type": "string"},
    "earliest": {"type": "string"},
    "latest": {"type": "string"},
    "explanation": {"type": "string"}
  },
  "required": ["spl", "earliest", "latest", "explanation"],
  "additionalProperties": false
}`

const systemPrompt = `You write one Splunk SPL search that answers an operator's question about a project's production logs.
You have no tools: read the log knowledge and the question, then answer with the JSON object only.
- spl: the base search terms plus optional pipes. Do NOT add the project's namespace filter: it is added for you.
  Use the field names from the knowledge (kubernetes.container_name, level, message, caller, prefix, request_id, …).
  Read-only and self-contained: no subsearches (no square brackets anywhere, also not in regexes), no join, append,
  union, lookup, inputlookup, map, rest, collect, outputlookup, sendemail, delete. Never start with a pipe.
  For "how many / which service / trend" questions end with stats, top or timechart; for "show me" questions return
  events (optionally | head N). Prefer exact fields (container, level, request_id, caller) over free text.
- earliest: Splunk relative time for the window the question implies: -15m, -1h, -4h, -24h, -7d, @d (today),
  -1d@d (yesterday, with latest @d). Default -1h. Never more than -7d.
- latest: "" for now, or a snap like @d.
- explanation: one or two short lines in the question's language: what the search finds and any assumption made.`

// Generator runs the generation turn.
type Generator struct {
	Config *config.Config
	// Run executes claude with args and stdin and returns stdout. Nil: exec Config.Claude.Bin.
	Run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// Context is what the model knows about the project's logs.
type Context struct {
	Project   string
	Filter    string // shown, not to be repeated
	Services  []string
	Knowledge string // the prod-logs markdown of the project
	Examples  []config.LogQuery
}

// Generate asks for a search answering question, then validates it.
func (g *Generator) Generate(ctx context.Context, lc Context, question string) (*Result, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("câu hỏi trống")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	run := g.Run
	if run == nil {
		run = g.exec
	}
	out, err := run(ctx, g.args(), prompt(lc, question))
	if err != nil {
		return nil, err
	}
	r, err := parse(out)
	if err != nil {
		return nil, err
	}
	return r, Check(r)
}

var relTime = regexp.MustCompile(`^(-\d+(s|m|h|d|w))?(@\w+)?$`)

// Check validates a generated (or edited) search and window.
func Check(r *Result) error {
	r.SPL = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.SPL), "search "))
	r.Earliest, r.Latest = strings.TrimSpace(r.Earliest), strings.TrimSpace(r.Latest)
	if r.Latest == "now" {
		r.Latest = ""
	}
	if r.SPL == "" {
		return errors.New("SPL trống")
	}
	if err := splunk.Validate(r.SPL); err != nil {
		return err
	}
	if r.Earliest == "" {
		r.Earliest = "-1h"
	}
	for _, t := range []string{r.Earliest, r.Latest} {
		if t != "" && !relTime.MatchString(t) {
			return fmt.Errorf("thời gian %q không hợp lệ (dạng -4h, @d, -1d@d)", t)
		}
	}
	if d, ok := splunk.Lookback(r.Earliest); ok && d > 7*24*time.Hour {
		return fmt.Errorf("khoảng thời gian %s quá 7 ngày", r.Earliest)
	}
	return nil
}

func (g *Generator) args() []string {
	c := g.Config.Claude
	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", schema,
		"--system-prompt", systemPrompt,
		"--tools", "",
		"--no-session-persistence",
		"--disable-slash-commands",
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if c.SettingSources != "" {
		args = append(args, "--setting-sources", c.SettingSources)
	}
	return args
}

func prompt(lc Context, question string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n", lc.Project)
	if lc.Filter != "" {
		fmt.Fprintf(&b, "Project filter (added automatically, do not repeat it): %s\n", lc.Filter)
	}
	if len(lc.Services) > 0 {
		fmt.Fprintf(&b, "Containers (kubernetes.container_name): %s\n", strings.Join(lc.Services, ", "))
	}
	if len(lc.Examples) > 0 {
		b.WriteString("\nSearches the team uses ({filter} is the project filter, {x} a parameter):\n")
		for _, q := range lc.Examples {
			fmt.Fprintf(&b, "- %s: %s\n", q.Name, q.SPL)
		}
	}
	if k := strings.TrimSpace(lc.Knowledge); k != "" {
		fmt.Fprintf(&b, "\nLog knowledge:\n<<<\n%s\n>>>\n", k)
	}
	fmt.Fprintf(&b, "\nQuestion: %s\n", question)
	return b.String()
}

func (g *Generator) exec(ctx context.Context, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.Config.Claude.Bin, args...)
	cmd.Dir = os.TempDir() // no project CLAUDE.md or settings of wherever taskpilot runs
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude quá thời gian (%s)", Timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300]) + "…"
		}
		return nil, fmt.Errorf("claude: %w: %s", err, msg)
	}
	return out, nil
}

// parse reads `--output-format json`: the result is in structured_output, or
// as JSON text in result on older CLIs.
func parse(out []byte) (*Result, error) {
	var res struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, fmt.Errorf("đọc kết quả claude: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("claude báo lỗi (%s): %s", res.Subtype, res.Result)
	}
	raw := []byte(res.StructuredOutput)
	if len(raw) == 0 || string(raw) == "null" {
		s := res.Result
		if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
			s = s[i : j+1]
		}
		raw = []byte(s)
	}
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("claude không trả về truy vấn hợp lệ")
	}
	return &r, nil
}
