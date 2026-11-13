// Package worklog drafts what is reported on Hasaki Work when a task is done:
// the name of the (sub)task and the work result ("Kết quả công việc"). One
// short Claude turn without tools reads what taskpilot recorded about the
// task (ticket, completion, pull requests, gate history) and writes both in
// the team's style; the operator reviews them before anything is sent.
package worklog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/oneshot"
)

// Timeout bounds one drafting turn.
const Timeout = 2 * time.Minute

// Input is what is known about the work.
type Input struct {
	Language   string   // operator language, e.g. Vietnamese
	Project    string   // project name
	Parent     string   // the Hasaki Work task the report goes under ("HSK-X — name"), "" for a new task
	Title      string   // the taskpilot task title
	Ticket     string   // ticket body (trimmed)
	Completion string   // the agent's completion summary
	PRs        []string // "repo → dest: title — url"
	Gates      []string // "plan-2 approved: summary | operator: comment"
	Examples   []string // names of the team's tasks in the same project (style)
	Subtask    bool     // the name is a subtask's
}

// Draft is the proposal.
type Draft struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

const schema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "result": {"type": "string"}
  },
  "required": ["name", "result"],
  "additionalProperties": false
}`

const systemPrompt = `You write the report of finished engineering work for a Hasaki Work task. You have no tools: read the material and answer with the JSON object only, in the operator's language.
- name: the task (or subtask) title, at most 90 characters. Follow the naming style of the examples: keep their tag prefixes such as [BE], [FE], [Issue Production], [Cải tiến] when they fit; say what was done, concretely (the change, not the ticket's question). For a subtask, name the part of the parent that this work delivered; do not repeat the parent's title.
- result: the work result a reviewer reads on Hasaki Work, plain text, 3 to 8 short lines: what changed and where (services/repos, endpoints, jobs), how it was verified (tests, QC), what remains or the risks if any, and the pull request links on their own lines at the end. Use only facts from the material; never invent numbers, links or test results. No markdown headings, no code fences.`

// Drafter runs the drafting turn.
type Drafter struct {
	Config *config.Config
	// Run executes claude with args and stdin and returns stdout. Nil: exec Config.Claude.Bin.
	Run func(ctx context.Context, args []string, stdin string) ([]byte, error)
}

func (d *Drafter) run(ctx context.Context, args []string, stdin string) ([]byte, error) {
	if d.Run != nil {
		return d.Run(ctx, args, stdin)
	}
	return oneshot.Exec(ctx, d.Config.Claude.Bin, args, stdin, Timeout)
}

// Draft asks for the name and the result.
func (d *Drafter) Draft(ctx context.Context, in Input) (*Draft, error) {
	if strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Completion) == "" {
		return nil, errors.New("không có gì để soạn: task chưa có tiêu đề hay tóm tắt hoàn thành")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	c := d.Config.Claude
	out, err := d.run(ctx, oneshot.Args(schema, systemPrompt, c.WorklogModel, c.SettingSources), Prompt(in))
	if err != nil {
		return nil, err
	}
	dr, err := parse(out)
	if err != nil {
		return nil, err
	}
	dr.Name = strings.TrimSpace(strings.Trim(strings.TrimSpace(dr.Name), `"`))
	dr.Result = strings.TrimSpace(dr.Result)
	if dr.Name == "" || dr.Result == "" {
		return nil, errors.New("claude trả về bản nháp rỗng")
	}
	if r := []rune(dr.Name); len(r) > 120 {
		dr.Name = string(r[:120])
	}
	return dr, nil
}

// Prompt lays out the material (exported for tests).
func Prompt(in Input) string {
	var b strings.Builder
	lang := in.Language
	if lang == "" {
		lang = "Vietnamese"
	}
	fmt.Fprintf(&b, "Operator language: %s\nProject: %s\n", lang, in.Project)
	if in.Subtask {
		fmt.Fprintf(&b, "Write a SUBTASK under the Hasaki Work task: %s\n", in.Parent)
	} else {
		b.WriteString("Write a new Hasaki Work task.\n")
	}
	if len(in.Examples) > 0 {
		b.WriteString("\nNames of the team's tasks in this project (style examples):\n")
		for _, e := range in.Examples {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	fmt.Fprintf(&b, "\nTaskpilot task title: %s\n", in.Title)
	if t := oneshot.Clip(in.Ticket, 3000); t != "" {
		fmt.Fprintf(&b, "\nTicket:\n<<<\n%s\n>>>\n", t)
	}
	if c := oneshot.Clip(in.Completion, 4000); c != "" {
		fmt.Fprintf(&b, "\nCompletion summary written by the agent:\n<<<\n%s\n>>>\n", c)
	}
	if len(in.PRs) > 0 {
		b.WriteString("\nPull requests:\n")
		for _, p := range in.PRs {
			fmt.Fprintf(&b, "- %s\n", oneshot.Clip(p, 600))
		}
	}
	if len(in.Gates) > 0 {
		b.WriteString("\nGate history (oldest first):\n")
		for _, g := range in.Gates {
			fmt.Fprintf(&b, "- %s\n", oneshot.Clip(g, 500))
		}
	}
	return b.String()
}

func parse(out []byte) (*Draft, error) {
	raw, _, err := oneshot.Answer(out)
	if err != nil {
		return nil, err
	}
	var d Draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, errors.New("claude không trả về bản nháp hợp lệ")
	}
	return &d, nil
}

// ---- task assessment -----------------------------------------------------------------

// AssessInput is what is known of a task the agent has not assessed.
type AssessInput struct {
	Language   string
	Project    string
	Title      string
	Ticket     string
	Completion string // the completion summary or the agent's last message, if any
}

// Assessment is the difficulty level and kind of a task.
type Assessment struct {
	Level  string `json:"level"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

const assessSchema = `{
  "type": "object",
  "properties": {
    "level": {"type": "string", "enum": ["light", "standard", "hard", "hardest"]},
    "kind": {"type": "string", "enum": ["feature", "bugfix", "investigation", "improvement", "refactor", "config", "test", "docs", "ops", "other"]},
    "reason": {"type": "string"}
  },
  "required": ["level", "kind", "reason"],
  "additionalProperties": false
}`

const assessSystem = `You assess an engineering task for the operator who supervises a coding agent. You have no tools: answer with the JSON object only.
level: light (mechanical or well-located change, one obvious fix, copy/config), standard (a normal feature or bug fix across a few files or one repo, clear criteria), hard (cross-repo, new service/module, unclear or long spec, concurrency, money/payment correctness, migrations, a production incident with unclear cause), hardest (novel design with many interacting parts, subtle correctness across systems).
kind: feature, bugfix, investigation (find a cause or answer a question, no change asked yet), improvement, refactor, config, test, docs, ops (operations: deploy, whitelist, data fix), other.
reason: one short sentence in the operator's language saying what in the task drives the level.`

// Assess asks for the level and kind of a task.
func (d *Drafter) Assess(ctx context.Context, in AssessInput) (*Assessment, error) {
	if strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Ticket) == "" {
		return nil, errors.New("task trống")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	c := d.Config.Claude
	args := oneshot.Args(assessSchema, assessSystem, c.Model, c.SettingSources)
	lang := in.Language
	if lang == "" {
		lang = "Vietnamese"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Operator language: %s\nProject: %s\nTask title: %s\n", lang, in.Project, in.Title)
	if t := oneshot.Clip(in.Ticket, 4000); t != "" {
		fmt.Fprintf(&b, "\nTicket:\n<<<\n%s\n>>>\n", t)
	}
	if t := oneshot.Clip(in.Completion, 2000); t != "" {
		fmt.Fprintf(&b, "\nWhat the agent reported so far:\n<<<\n%s\n>>>\n", t)
	}
	out, err := d.run(ctx, args, b.String())
	if err != nil {
		return nil, err
	}
	raw, _, err := oneshot.Answer(out)
	if err != nil {
		return nil, err
	}
	var a Assessment
	if err := json.Unmarshal(raw, &a); err != nil || a.Level == "" || a.Kind == "" {
		return nil, errors.New("claude không trả về đánh giá hợp lệ")
	}
	return &a, nil
}
