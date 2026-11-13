// Package hook implements the PreToolUse / preToolUse gate hook for Claude
// Code and Cursor Agent.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// FormatClaude and FormatCursor select the hook response schema.
const (
	FormatClaude = "claude"
	FormatCursor = "cursor"
)

// input is the subset of the PreToolUse / preToolUse payload the hook needs.
type input struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Cwd           string          `json:"cwd"`
}

// writeTools change files; they are confined to the task workspace.
var writeTools = map[string]bool{
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "Delete": true,
}

type claudeOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

type cursorOutput struct {
	Permission   string `json:"permission"`
	AgentMessage string `json:"agent_message,omitempty"`
	UserMessage  string `json:"user_message,omitempty"`
}

// DetectFormat returns FormatCursor when the payload looks like Cursor's
// preToolUse hook, otherwise FormatClaude.
func DetectFormat(hookEventName string) string {
	if hookEventName == "preToolUse" {
		return FormatCursor
	}
	return FormatClaude
}

// Run reads one PreToolUse payload from r and, when the call must be blocked,
// writes a deny decision to w. format is "claude", "cursor", or "" (auto).
// Claude: allowed calls produce no output. Cursor: allowed calls return
// {"permission":"allow"} (invalid/empty JSON fails closed on Cursor).
// Any internal error denies (fail closed).
func Run(r io.Reader, w io.Writer, st *store.Store, taskID string, pol *gate.Policy, format string) error {
	return RunWith(r, w, st, taskID, pol, format, Options{})
}

// Options are the optional behaviours of the hook.
type Options struct {
	// Graphify: the task's project has code graphs. The first code search
	// of a run without a graph query is turned back once with a reminder to
	// ask the graph (the next search goes through).
	Graphify bool
}

// RunWith is Run with options.
func RunWith(r io.Reader, w io.Writer, st *store.Store, taskID string, pol *gate.Policy, format string, opts Options) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return deny(w, formatOr(format, FormatClaude), fmt.Sprintf("taskpilot hook could not read its input (%v); blocked to be safe", err))
	}
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return deny(w, formatOr(format, FormatClaude), fmt.Sprintf("taskpilot hook could not parse its input (%v); blocked to be safe", err))
	}
	if format == "" {
		format = DetectFormat(in.HookEventName)
	}
	if !store.ValidID(taskID) || !st.Exists(taskID) {
		return deny(w, format, fmt.Sprintf("taskpilot hook: unknown task %q; blocked to be safe", taskID))
	}
	if in.ToolName == "AskUserQuestion" {
		return deny(w, format, forwardQuestions(st, taskID, in.ToolInput))
	}
	if writeTools[in.ToolName] {
		if reason := outsideWorkspace(st, taskID, in); reason != "" {
			return deny(w, format, reason)
		}
	}
	v := pol.Evaluate(gate.Call{Tool: in.ToolName, Input: in.ToolInput}, func(g string) (bool, error) {
		return st.GateApproved(taskID, g)
	})
	if v.Allow {
		if opts.Graphify && isCodeSearch(in) && st.GraphifyNudge(taskID) {
			tool := "mcp__taskpilot__graphify"
			if format == FormatCursor {
				tool = "graphify"
			}
			return deny(w, format, graphifyReminder(tool))
		}
		return allow(w, format)
	}
	return deny(w, format, v.Reason)
}

// codeSearchCmd matches a shell search of code.
var codeSearchCmd = regexp.MustCompile(`(^|[;&|(]\s*|\s)(rg|grep|egrep|ag|ack|git\s+(-C\s+\S+\s+)?grep)\s`)

// isCodeSearch reports whether a call searches code for text.
func isCodeSearch(in input) bool {
	switch in.ToolName {
	case "Grep":
		return true
	case "Bash", "Shell":
		var b struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(in.ToolInput, &b)
		return codeSearchCmd.MatchString(b.Command)
	}
	return false
}

func graphifyReminder(tool string) string {
	load := ""
	if strings.HasPrefix(tool, "mcp__") {
		load = " (deferred: load it with ToolSearch \"select:" + tool + "\")"
	}
	return "taskpilot reminder (once per run, not an error): ask the code graph before grepping. " + tool + load +
		" covers every repo of the project and your branch, and joins the repos by their gRPC calls: " +
		`{"action":"links","text":"<method / service / function>"} for who serves and who calls an RPC, ` +
		`{"action":"affected","text":"<function>"} for its callers across repos, {"action":"query","text":"<question>"} ` +
		"for where something lives. Then search again to confirm: your next search is allowed."
}

func formatOr(format, fallback string) string {
	if format == "" {
		return fallback
	}
	return format
}

// forwardQuestions turns a Claude Code AskUserQuestion call, which cannot be
// answered in a headless session, into a question request for the operator
// (the same as the ask_operator tool). The call itself is always denied; the
// reason tells the agent what happened.
func forwardQuestions(st *store.Store, taskID string, raw json.RawMessage) string {
	var in struct {
		Questions []store.Question `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Questions) == 0 {
		return "taskpilot: this session is headless; ask the operator with the taskpilot ask_operator tool (questions with options), then end your turn"
	}
	if p, err := st.PendingRequest(taskID); err != nil {
		return "taskpilot: could not check pending requests: " + err.Error()
	} else if p != nil {
		return fmt.Sprintf("taskpilot: request %s is still waiting for the operator; end your turn now", p.ID)
	}
	summary := in.Questions[0].Question
	r, err := st.AddRequestWithQuestions(taskID, store.QuestionGate, summary, "", in.Questions)
	if err != nil {
		return "taskpilot: your questions were not sent (" + err.Error() + "); fix them and use the ask_operator tool"
	}
	return fmt.Sprintf("taskpilot: this session is headless, so your question(s) were sent to the operator as request %s "+
		"with the same options. Do not wait for an answer here: end your turn now with a one-line status; the session "+
		"resumes with the operator's answers.", r.ID)
}

// outsideWorkspace returns a denial reason when a write tool targets a file
// outside the task's workspace (e.g. the operator's own checkout).
func outsideWorkspace(st *store.Store, taskID string, in input) string {
	t, err := st.LoadTask(taskID)
	if err != nil || t.Workspace == "" {
		return "taskpilot hook: the task has no workspace yet; blocked to be safe"
	}
	var args struct {
		FilePath     string `json:"file_path"`
		Path         string `json:"path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(in.ToolInput, &args)
	target := args.FilePath
	if target == "" {
		target = args.Path
	}
	if target == "" {
		target = args.NotebookPath
	}
	if target == "" {
		return "taskpilot hook: could not tell which file this write targets; blocked to be safe"
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(in.Cwd, target)
	}
	target = filepath.Clean(target)
	ws := filepath.Clean(t.Workspace)
	// Resolve symlinks where possible so a link inside the workspace (the file
	// itself, or its folder for a new file) cannot point out of it.
	if r, err := filepath.EvalSymlinks(target); err == nil {
		target = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
		target = filepath.Join(r, filepath.Base(target))
	}
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	if target != ws && !strings.HasPrefix(target, ws+string(filepath.Separator)) {
		return fmt.Sprintf("taskpilot policy: writes are only allowed inside the task workspace %s (open the repo you need with open_repo); %s is outside it", t.Workspace, target)
	}
	return ""
}

func allow(w io.Writer, format string) error {
	if format != FormatCursor {
		return nil
	}
	return json.NewEncoder(w).Encode(cursorOutput{Permission: "allow"})
}

func deny(w io.Writer, format, reason string) error {
	if format == FormatCursor {
		return json.NewEncoder(w).Encode(cursorOutput{
			Permission:   "deny",
			AgentMessage: reason,
			UserMessage:  reason,
		})
	}
	return Deny(w, reason)
}

// Deny writes a Claude Code PreToolUse deny decision.
func Deny(w io.Writer, reason string) error {
	var o claudeOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = "deny"
	o.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(w).Encode(o)
}
