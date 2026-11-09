// Package gate evaluates the tool-call policy that enforces approval gates.
//
// Gates are enforced here, in the harness, not in the prompt: the agent is
// told about them in its skill, but a PreToolUse hook backed by this package
// blocks the call regardless of what the model decides.
package gate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// Policy is a compiled rule set.
type Policy struct{ rules []compiled }

type compiled struct {
	src     config.Rule
	tool    *regexp.Regexp
	command *regexp.Regexp
	path    *regexp.Regexp
}

// Compile validates and compiles rules.
func Compile(rules []config.Rule) (*Policy, error) {
	p := &Policy{}
	for i, r := range rules {
		tool, err := regexp.Compile(`^(?:` + r.Tool + `)$`)
		if err != nil {
			return nil, fmt.Errorf("rules[%d].tool: %w", i, err)
		}
		c := compiled{src: r, tool: tool}
		if r.Command != "" {
			if c.command, err = regexp.Compile(r.Command); err != nil {
				return nil, fmt.Errorf("rules[%d].command: %w", i, err)
			}
		}
		if r.Path != "" {
			if c.path, err = regexp.Compile(r.Path); err != nil {
				return nil, fmt.Errorf("rules[%d].path: %w", i, err)
			}
		}
		p.rules = append(p.rules, c)
	}
	return p, nil
}

// Call is the part of a tool call the policy looks at.
type Call struct {
	Tool  string
	Input json.RawMessage
}

// Verdict is the policy's decision on a call.
type Verdict struct {
	Allow  bool
	Reason string
}

// ApprovedFunc reports whether a gate has been approved for the current task.
type ApprovedFunc func(gate string) (bool, error)

// Evaluate checks every matching rule; any denial blocks the call.
// Errors while checking approvals fail closed.
func (p *Policy) Evaluate(c Call, approved ApprovedFunc) Verdict {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
		Pattern      string `json:"pattern"`
	}
	if len(c.Input) > 0 {
		_ = json.Unmarshal(c.Input, &in)
	}
	command := in.Command
	paths := []string{in.FilePath, in.Path, in.NotebookPath}
	if c.Tool == "Glob" {
		paths = append(paths, in.Pattern) // a Glob pattern can be an absolute path
	}
	var reasons []string
	for _, r := range p.rules {
		if !r.tool.MatchString(c.Tool) {
			continue
		}
		if r.command != nil && !r.command.MatchString(command) {
			continue
		}
		if r.path != nil && !anyMatch(r.path, paths) {
			continue
		}
		if r.src.Deny != "" {
			reasons = append(reasons, r.src.Deny)
			continue
		}
		ok, err := approved(r.src.Requires)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("taskpilot could not check gate %q (%v); blocked to be safe", r.src.Requires, err))
			continue
		}
		if !ok {
			reasons = append(reasons, fmt.Sprintf(
				"taskpilot gate %q is not approved yet. Call the request_approval tool with gate=%q, then end your turn and wait for the operator.",
				r.src.Requires, r.src.Requires))
		}
	}
	if len(reasons) > 0 {
		return Verdict{Allow: false, Reason: strings.Join(reasons, "\n")}
	}
	return Verdict{Allow: true}
}

func anyMatch(re *regexp.Regexp, values []string) bool {
	for _, v := range values {
		if v != "" && re.MatchString(v) {
			return true
		}
	}
	return false
}
