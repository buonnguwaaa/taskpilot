// Package hook implements the Claude Code PreToolUse hook that enforces gates.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// input is the subset of the PreToolUse payload the hook needs.
type input struct {
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	Cwd           string          `json:"cwd"`
}

// writeTools change files; they are confined to the task workspace.
var writeTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

type output struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// Run reads one PreToolUse payload from r and, when the call must be blocked,
// writes a deny decision to w. Allowed calls produce no output, which leaves
// the normal permission flow in charge. Any internal error denies (fail closed).
func Run(r io.Reader, w io.Writer, st *store.Store, taskID string, pol *gate.Policy) error {
	var in input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return Deny(w, fmt.Sprintf("taskpilot hook could not parse its input (%v); blocked to be safe", err))
	}
	if !store.ValidID(taskID) || !st.Exists(taskID) {
		return Deny(w, fmt.Sprintf("taskpilot hook: unknown task %q; blocked to be safe", taskID))
	}
	if writeTools[in.ToolName] {
		if reason := outsideWorkspace(st, taskID, in); reason != "" {
			return Deny(w, reason)
		}
	}
	v := pol.Evaluate(gate.Call{Tool: in.ToolName, Input: in.ToolInput}, func(g string) (bool, error) {
		return st.GateApproved(taskID, g)
	})
	if v.Allow {
		return nil
	}
	return Deny(w, v.Reason)
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
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(in.ToolInput, &args)
	target := args.FilePath
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
	// Resolve symlinks where possible so a link inside the workspace cannot point out of it.
	if r, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
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

// Deny writes a PreToolUse deny decision.
func Deny(w io.Writer, reason string) error {
	var o output
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = "deny"
	o.HookSpecificOutput.PermissionDecisionReason = reason
	return json.NewEncoder(w).Encode(o)
}
