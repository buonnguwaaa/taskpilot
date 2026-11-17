package hook

import (
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Grants: rules the operator allowed while the task ran (request_permission).
// Claude Code only knows the allowlist it started with, so the hook allows a
// call explicitly when the grants (with the allowlist) cover it.
//
// Bash commands are split on && || ; | and newlines, and every part must
// match a rule. A command substitution, a backquote or a redirection is never
// covered: the call then goes to Claude Code's own allowlist as before (and is
// denied when it is not there). The split ignores quotes, so a quoted
// separator only makes a part that matches nothing — never a wider allow.

// ruleRe parses "Tool" or "Tool(pattern)".
var ruleRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\((.*)\))?$`)

// ValidRule reports whether a rule is well formed and narrow enough to grant:
// a tool name, or Bash with a command (not Bash, Bash(*)).
func ValidRule(rule string) bool {
	m := ruleRe.FindStringSubmatch(strings.TrimSpace(rule))
	if m == nil {
		return false
	}
	if m[1] == "Bash" || m[1] == "Shell" {
		p := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(m[2], "*"), ":"))
		return p != "" && !strings.ContainsAny(p, "*`$><;&|")
	}
	return true
}

var unsafeShell = regexp.MustCompile("\\$\\(|`|[<>]")

// covered reports whether every part of the call matches one of the rules.
func covered(tool string, input json.RawMessage, rules []string) bool {
	if tool == "Bash" || tool == "Shell" {
		var b struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &b) != nil || strings.TrimSpace(b.Command) == "" || unsafeShell.MatchString(b.Command) {
			return false
		}
		for _, part := range splitShell(b.Command) {
			if !anyBash(part, rules) {
				return false
			}
		}
		return true
	}
	for _, r := range rules {
		m := ruleRe.FindStringSubmatch(r)
		if m == nil || m[1] != tool && !strings.HasPrefix(tool, m[1]+"__") {
			continue
		}
		if m[2] == "" {
			return true
		}
		if d, ok := strings.CutPrefix(m[2], "domain:"); ok && tool == "WebFetch" {
			var w struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(input, &w) == nil {
				if u, err := url.Parse(w.URL); err == nil && (u.Hostname() == d || strings.HasSuffix(u.Hostname(), "."+d)) {
					return true
				}
			}
		}
	}
	return false
}

var shellSep = regexp.MustCompile(`&&|\|\||[;|&\n]`)

func splitShell(cmd string) []string {
	var out []string
	for _, p := range shellSep.Split(cmd, -1) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// anyBash reports whether one command matches a Bash rule: "Bash(x *)" or
// "Bash(x:*)" is a prefix, "*" elsewhere a wildcard, anything else exact.
func anyBash(cmd string, rules []string) bool {
	for _, r := range rules {
		m := ruleRe.FindStringSubmatch(r)
		if m == nil || m[1] != "Bash" && m[1] != "Shell" || m[2] == "" {
			continue
		}
		p := strings.Replace(m[2], ":*", " *", 1)
		if bashMatch(p, cmd) {
			return true
		}
	}
	return false
}

func bashMatch(pattern, cmd string) bool {
	if !strings.Contains(pattern, "*") {
		return cmd == pattern
	}
	// "curl *" also matches a bare "curl".
	if strings.HasSuffix(pattern, " *") && cmd == strings.TrimSuffix(pattern, " *") {
		return true
	}
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	ok, _ := regexp.MatchString(`^(?s)`+strings.Join(parts, ".*")+`$`, cmd)
	return ok
}

// grantAllow is an explicit allow (Claude Code would deny a call outside the
// allowlist it started with).
func grantAllow(w io.Writer, format, rule string) error {
	if format == FormatCursor {
		return json.NewEncoder(w).Encode(cursorOutput{Permission: "allow"})
	}
	var o claudeOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = "allow"
	o.HookSpecificOutput.PermissionDecisionReason = "allowed by the operator: " + rule
	return json.NewEncoder(w).Encode(o)
}
