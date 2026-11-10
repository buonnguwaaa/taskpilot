// Package intake turns a free-form request typed by the operator ("fix the
// export of MO in bom-api, it times out over 10k rows") into a ticket draft:
// project, starting repo, title and body. One short headless `claude -p` turn
// with no tools does the reading; the operator always reviews the draft
// before it reaches the inbox.
package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/source"
)

// Timeout bounds one drafting turn.
const Timeout = 3 * time.Minute

// Draft is what the model proposes. Project and Repo are validated against the
// config; Note explains a guess or a missing piece to the operator.
type Draft struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Note    string `json:"note"`
}

// Ticket builds the inbox ticket: the drafted body followed by the operator's
// request verbatim, so nothing the operator wrote is lost in the rewrite.
func (d *Draft) Ticket(prompt string) source.Ticket {
	body := strings.TrimSpace(d.Body)
	if p := strings.TrimSpace(prompt); p != "" && p != body {
		if body != "" {
			body += "\n\n"
		}
		body += "Operator's original request:\n" + p
	}
	return source.Ticket{Project: d.Project, Repo: d.Repo, Title: d.Title, Body: body}
}

const schema = `{
  "type": "object",
  "properties": {
    "project": {"type": "string", "description": "key of the project, or empty when unclear"},
    "repo": {"type": "string", "description": "repo of that project to open first, or empty"},
    "title": {"type": "string", "description": "short imperative ticket title"},
    "body": {"type": "string", "description": "ticket body: context, what to do, acceptance criteria"},
    "note": {"type": "string", "description": "one line for the operator: what was guessed or is missing; empty if nothing"}
  },
  "required": ["project", "repo", "title", "body", "note"],
  "additionalProperties": false
}`

const systemPrompt = `You turn an operator's free-form request into a ticket for an autonomous coding agent.
You do not do the work and you have no tools: read the request and the project catalog, then answer with the JSON object only.
- project: the catalog key the request belongs to. Use "" when the request fits none or several equally.
- repo: one repo key of that project to open first, only if the request clearly points to it; otherwise "".
- title: imperative, at most 80 characters, in the request's language.
- body: rewrite the request into a clear ticket in the request's language: context, what to change, and
  "Acceptance criteria:" as a short list. Do not invent facts, file names or numbers that are not in the request;
  write open questions as such.
- note: one short line telling the operator what you guessed or what is missing, in the request's language; "" if nothing.`

// Drafter runs the drafting turn.
type Drafter struct {
	Config *config.Config
	// Run executes claude with args and stdin and returns stdout. Nil: exec Config.Claude.Bin.
	Run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

// Draft asks the model for a ticket draft and validates it against the config.
// An unknown project or repo is cleared (with a note) rather than rejected, so
// the operator can pick it in the form.
func (d *Drafter) Draft(ctx context.Context, prompt string) (*Draft, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("prompt trống")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	run := d.Run
	if run == nil {
		run = d.exec
	}
	out, err := run(ctx, d.args(), d.userPrompt(prompt))
	if err != nil {
		return nil, err
	}
	dr, err := parse(out)
	if err != nil {
		return nil, err
	}
	d.validate(dr)
	return dr, nil
}

func (d *Drafter) args() []string {
	c := d.Config.Claude
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

// userPrompt lists the projects and repos (with their descriptions) so the
// model can map the request to them.
func (d *Drafter) userPrompt(prompt string) string {
	var b strings.Builder
	b.WriteString("Project catalog:\n")
	for _, name := range d.Config.ProjectNames() {
		p := d.Config.Projects[name]
		fmt.Fprintf(&b, "- project %q", name)
		if p.Name != "" && p.Name != name {
			fmt.Fprintf(&b, " (%s)", p.Name)
		}
		b.WriteString("\n")
		for _, r := range p.RepoNames() {
			fmt.Fprintf(&b, "  - repo %q", r)
			if desc := strings.TrimSpace(p.Repos[r].Description); desc != "" {
				b.WriteString(": ")
				b.WriteString(strings.Join(strings.Fields(desc), " "))
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\nOperator's request:\n<<<\n%s\n>>>\n", prompt)
	return b.String()
}

func (d *Drafter) exec(ctx context.Context, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.Config.Claude.Bin, args...)
	// A neutral directory: no project CLAUDE.md or settings of wherever taskpilot was started.
	cmd.Dir = os.TempDir()
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
		return nil, fmt.Errorf("claude: %w: %s", err, truncate(msg, 300))
	}
	return out, nil
}

// parse reads `--output-format json`: the draft is in structured_output, or,
// on older CLIs, as JSON text in result.
func parse(out []byte) (*Draft, error) {
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
		return nil, fmt.Errorf("claude báo lỗi (%s): %s", res.Subtype, truncate(res.Result, 300))
	}
	raw := []byte(res.StructuredOutput)
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte(extractObject(res.Result))
	}
	var dr Draft
	if err := json.Unmarshal(raw, &dr); err != nil {
		return nil, fmt.Errorf("claude không trả về bản nháp hợp lệ: %s", truncate(res.Result, 200))
	}
	dr.Project, dr.Repo = strings.TrimSpace(dr.Project), strings.TrimSpace(dr.Repo)
	dr.Title, dr.Body, dr.Note = strings.TrimSpace(dr.Title), strings.TrimSpace(dr.Body), strings.TrimSpace(dr.Note)
	if dr.Title == "" && dr.Body == "" {
		return nil, errors.New("claude trả về bản nháp rỗng")
	}
	return &dr, nil
}

// extractObject returns the outermost {...} of s (tolerates code fences).
func extractObject(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return s
	}
	return s[i : j+1]
}

func (d *Drafter) validate(dr *Draft) {
	addNote := func(s string) {
		if dr.Note != "" {
			dr.Note += " · "
		}
		dr.Note += s
	}
	p, ok := d.Config.Projects[dr.Project]
	if dr.Project != "" && !ok {
		addNote(fmt.Sprintf("project %q không có trong config", dr.Project))
		dr.Project = ""
	}
	if dr.Repo == "" {
		return
	}
	if _, ok := p.Repos[dr.Repo]; !ok || dr.Project == "" {
		if dr.Project != "" {
			addNote(fmt.Sprintf("repo %q không thuộc %s", dr.Repo, dr.Project))
		}
		dr.Repo = ""
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
