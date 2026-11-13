// Package runner launches one headless agent turn for a task (Claude Code or
// Cursor Agent CLI) and records its stream-json events.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// Runner holds what every run needs.
type Runner struct {
	Config *config.Config
	// Conf, when set, returns the current config (the daemon reloads it when
	// the file changes); Config is used otherwise.
	Conf      func() *config.Config
	Store     *store.Store
	PluginDir string
	// Exe is the taskpilot binary, used for the MCP server and the hook.
	Exe string
}

func (r *Runner) cfg() *config.Config {
	if r.Conf != nil {
		return r.Conf()
	}
	return r.Config
}

// Result summarizes one run.
type Result struct {
	SessionID        string
	CostUSD          float64
	IsError          bool
	Subtype          string
	Text             string
	NumTurns         int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ToolCalls        []store.ToolCall
	Quota            Quota
}

// systemContract is appended to Claude Code's system prompt for every run.
const systemContract = `You are an autonomous engineer running headless under taskpilot. No human reads this terminal.
Follow the taskpilot:workflow skill for the whole task. Talk to the operator only through the taskpilot MCP tools
(request_approval, complete_task) and a short final status line per turn. Write operator-facing text in %s.`

// graphifyNote tells every run (resumes too, which do not reread the
// workflow skill) to ask the code graph before grepping. tool is the name
// the worker sees.
func (r *Runner) graphifyNote(project, tool string) string {
	cfg := r.cfg()
	if cfg.Graphify == nil || len(cfg.ProjectGraphLines(project)) == 0 {
		return ""
	}
	load := ""
	if strings.HasPrefix(tool, "mcp__") {
		load = fmt.Sprintf(" It is a deferred tool: load it first with ToolSearch \"select:%s\".", tool)
	}
	return fmt.Sprintf(`

Code graph: %s answers from graphify graphs of ALL repos of this project, joined by the gRPC calls between
them, for your branch (line task: your worktrees with your edits), release/prod (prod) and release/qc (qc).%s
Ask it BEFORE grepping whenever the question is where something lives, who calls it, which repo serves an RPC,
or what a change affects: action links (gRPC between repos), affected (callers, across repos), path, explain,
query, read (code of release/qc or release/prod). Then Grep/Read only to confirm what it points to.`, tool, load)
}

// cursorAgentsMD is written into the task workspace for Cursor Agent (no
// --append-system-prompt flag).
const cursorAgentsMD = `You are an autonomous engineer running headless under taskpilot. No human reads this terminal.
Follow the taskpilot workflow skill (and ponytail) for the whole task. Talk to the operator only through the
taskpilot MCP tools (request_approval, complete_task, ask_operator) and a short final status line per turn.
Write operator-facing text in %s.

Cursor tool names: use Write/Delete for files, Shell for commands, and MCP tools named like request_approval.
Gate policy is enforced by a preToolUse hook — if a call is denied, request the named gate and end your turn.
`

// KindFor is the headless worker used for this task: sticky task worker,
// home-wide Claude quota latch, or config default.
func (r *Runner) KindFor(t *store.Task) string {
	if r.Store != nil {
		if w, err := r.Store.TaskWorker(t.ID); err == nil && w != nil && w.Agent != "" {
			return w.Agent
		}
		if r.cfg().AgentKind() == config.AgentClaude && r.cfg().Cursor.FallbackEnabled() {
			if latch, err := r.Store.ClaudeQuotaLatch(); err == nil && latch != nil {
				if r.CursorAvailable() {
					return config.AgentCursor
				}
			}
		}
	}
	return r.cfg().AgentKind()
}

// CursorAvailable reports whether the Cursor Agent binary can be executed.
func (r *Runner) CursorAvailable() bool {
	bin := r.BinFor(config.AgentCursor)
	if _, err := exec.LookPath(bin); err == nil {
		return true
	}
	_, err := os.Stat(bin)
	return err == nil
}

// BinFor returns the binary for kind.
func (r *Runner) BinFor(kind string) string {
	if kind == config.AgentCursor {
		if r.cfg().Cursor.Bin != "" {
			return r.cfg().Cursor.Bin
		}
		return "agent"
	}
	return r.cfg().AgentBin()
}

// Run executes one turn with KindFor(t).
func (r *Runner) Run(ctx context.Context, t *store.Task, prompt string, resume bool) (*Result, error) {
	return r.RunKind(ctx, t, prompt, resume, r.KindFor(t))
}

// RunKind executes one turn with an explicit worker kind (claude or cursor).
func (r *Runner) RunKind(ctx context.Context, t *store.Task, prompt string, resume bool, kind string) (*Result, error) {
	proj, ok := r.cfg().Projects[t.Project]
	if !ok {
		return nil, fmt.Errorf("unknown project %q", t.Project)
	}
	bin := r.BinFor(kind)
	args, err := r.args(t, proj, resume, kind)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = t.Workspace
	// Own process group: on stop, SIGTERM reaches the agent and everything it
	// started (shell tool commands, MCP servers) so nothing is orphaned and
	// the agent can save the session; after WaitDelay the pipes are closed anyway.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		"TASKPILOT_HOME="+r.Store.Home,
		"TASKPILOT_TASK_ID="+t.ID,
		"TASKPILOT_CONFIG="+r.cfg().Path,
	)
	cmd.Stdin = strings.NewReader(prompt)

	if err := os.MkdirAll(filepath.Dir(r.Store.StderrPath(t.ID, t.Runs)), 0o755); err != nil {
		return nil, err
	}
	stderrPath := r.Store.StderrPath(t.ID, t.Runs)
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd.Stderr = stderr

	events, err := os.OpenFile(r.Store.EventsPath(t.ID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer events.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	res := &Result{SessionID: t.SessionID}
	tc := &toolCalls{byID: map[string]int{}}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		events.Write(append(append([]byte{}, line...), '\n'))
		parseEvent(line, res)
		tc.add(line)
	}
	res.ToolCalls = tc.calls
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if b, err := os.ReadFile(stderrPath); err == nil && len(b) > 0 {
		observeQuotaText(string(b), "stderr", &res.Quota)
	}
	if waitErr != nil {
		observeQuotaText(waitErr.Error(), "exit", &res.Quota)
		return res, fmt.Errorf("%s exited: %w (see %s)", bin, waitErr, stderrPath)
	}
	if scanErr != nil {
		return res, fmt.Errorf("read %s output: %w", bin, scanErr)
	}
	return res, nil
}

// Profile is the model and effort the task's next run uses: the task's own
// profile (picked by the agent or pinned by the operator), else the profile
// of its assessed level (claude.level_profiles), over the config default.
func (r *Runner) Profile(t *store.Task) (model, effort string) {
	c := r.cfg().Claude
	model, effort = c.Model, c.Effort
	if r.KindFor(t) == config.AgentCursor && r.cfg().Cursor.Model != "" {
		model = r.cfg().Cursor.Model
	}
	if r.Store == nil {
		return model, effort
	}
	p, err := r.Store.TaskProfile(t.ID)
	if err == nil && p != nil {
		if p.Model != "" {
			model = p.Model
		}
		if p.Effort != "" {
			effort = p.Effort
		}
		return model, effort
	}
	// No profile yet: the one of the task's assessed level (Claude only:
	// the level models are Claude's).
	if a, _ := r.Store.TaskAssessment(t.ID); a != nil && r.KindFor(t) != config.AgentCursor {
		if lp, ok := c.ForLevel(a.Level); ok {
			if lp.Model != "" {
				model = lp.Model
			}
			if lp.Effort != "" {
				effort = lp.Effort
			}
		}
	}
	return model, effort
}

// Args builds the agent command line for KindFor(t). The prompt is passed on
// stdin, so no positional argument can be swallowed by a variadic flag.
func (r *Runner) Args(t *store.Task, proj config.Project, resume bool) ([]string, error) {
	return r.args(t, proj, resume, r.KindFor(t))
}

func (r *Runner) args(t *store.Task, proj config.Project, resume bool, kind string) ([]string, error) {
	if kind == config.AgentCursor {
		return r.cursorArgs(t, proj, resume)
	}
	return r.claudeArgs(t, proj, resume)
}

func (r *Runner) claudeArgs(t *store.Task, proj config.Project, resume bool) ([]string, error) {
	c := r.cfg().Claude
	mcpCfg, err := r.mcpConfigJSON(t, proj)
	if err != nil {
		return nil, err
	}
	settings, err := r.claudeSettings(t)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", c.PermissionMode,
		"--permission-prompts", "none",
		"--plugin-dir", r.PluginDir,
		"--settings", settings,
		"--strict-mcp-config",
		"--mcp-config", mcpCfg,
		"--append-system-prompt", fmt.Sprintf(systemContract, r.cfg().OperatorLanguage) + r.graphifyNote(t.Project, "mcp__taskpilot__graphify"),
		"--name", "taskpilot " + t.ID,
	}
	if c.MaxBudgetUSD > 0 { // no cap unless the operator sets one
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", c.MaxBudgetUSD))
	}
	model, effort := r.Profile(t)
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if c.SettingSources != "" {
		args = append(args, "--setting-sources", c.SettingSources)
	}
	args = append(args, c.ExtraArgs...)
	if resume {
		args = append(args, "--resume", t.SessionID)
		// After the operator took a run back: continue from the entry before it.
		if r.Store != nil {
			if at := r.Store.ResumeAt(t.ID); at != "" {
				args = append(args, "--resume-session-at", at)
			}
		}
	} else {
		args = append(args, "--session-id", t.SessionID)
	}
	// Last, because it is variadic.
	args = append(args, "--allowedTools", strings.Join(proj.AllowedToolList(), ","))
	return args, nil
}

func (r *Runner) cursorArgs(t *store.Task, proj config.Project, resume bool) ([]string, error) {
	if t.Workspace == "" {
		return nil, fmt.Errorf("task %s has no workspace", t.ID)
	}
	if err := r.prepareCursorWorkspace(t, proj); err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--force",
		"--approve-mcps",
		"--trust",
		"--sandbox", "disabled",
		"--workspace", t.Workspace,
		"--plugin-dir", r.PluginDir,
	}
	model, _ := r.Profile(t)
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, r.cfg().Cursor.ExtraArgs...)
	if resume {
		if t.SessionID == "" {
			return nil, fmt.Errorf("task %s has no Cursor session to resume", t.ID)
		}
		args = append(args, "--resume", t.SessionID)
	}
	return args, nil
}

func (r *Runner) mcpServers(t *store.Task, proj config.Project) map[string]config.MCPServer {
	servers := map[string]config.MCPServer{
		"taskpilot": {
			Command: r.Exe,
			Args:    []string{"mcp", "--config", r.cfg().Path, "--task", t.ID},
		},
	}
	for name, s := range proj.MCPServers {
		servers[name] = s
	}
	return servers
}

func (r *Runner) mcpConfigJSON(t *store.Task, proj config.Project) (string, error) {
	b, err := json.Marshal(map[string]any{"mcpServers": r.mcpServers(t, proj)})
	return string(b), err
}

func (r *Runner) claudeSettings(t *store.Task) (string, error) {
	hookCmd := shellQuote(r.Exe) + " hook --config " + shellQuote(r.cfg().Path) + " --task " + shellQuote(t.ID)
	s := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": hookCmd, "timeout": 30},
					},
				},
			},
		},
	}
	b, err := json.Marshal(s)
	return string(b), err
}

// prepareCursorWorkspace writes MCP, hooks, and AGENTS.md into the task
// workspace so Cursor Agent CLI picks them up for this run.
func (r *Runner) prepareCursorWorkspace(t *store.Task, proj config.Project) error {
	dir := filepath.Join(t.Workspace, ".cursor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mcp := map[string]any{"mcpServers": r.mcpServers(t, proj)}
	mb, err := json.MarshalIndent(mcp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	hookCmd := shellQuote(r.Exe) + " hook --format cursor --config " + shellQuote(r.cfg().Path) + " --task " + shellQuote(t.ID)
	hooks := map[string]any{
		"version": 1,
		"hooks": map[string]any{
			"preToolUse": []any{
				map[string]any{
					"command":    hookCmd,
					"failClosed": true,
					"timeout":    30,
				},
			},
		},
	}
	hb, err := json.MarshalIndent(hooks, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), append(hb, '\n'), 0o644); err != nil {
		return err
	}
	agents := fmt.Sprintf(cursorAgentsMD, r.cfg().OperatorLanguage) + r.graphifyNote(t.Project, "graphify")
	return os.WriteFile(filepath.Join(t.Workspace, "AGENTS.md"), []byte(agents), 0o644)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// streamEvent is the subset of stream-json events the runner reads.
type streamEvent struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	NumTurns     int             `json:"num_turns"`
	Usage        json.RawMessage `json:"usage"`
}

func parseEvent(line []byte, res *Result) {
	observeQuotaEvent(line, &res.Quota)
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	if ev.SessionID != "" {
		res.SessionID = ev.SessionID
	}
	if ev.Type == "result" {
		res.Subtype = ev.Subtype
		res.IsError = ev.IsError
		res.Text = ev.Result
		res.CostUSD = ev.TotalCostUSD
		res.NumTurns = ev.NumTurns
		parseUsage(ev.Usage, res)
	}
}

func parseUsage(raw json.RawMessage, res *Result) {
	if len(raw) == 0 {
		return
	}
	var u struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		InputTokensCamel         int64 `json:"inputTokens"`
		OutputTokensCamel        int64 `json:"outputTokens"`
		CacheReadTokens          int64 `json:"cacheReadTokens"`
		CacheWriteTokens         int64 `json:"cacheWriteTokens"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	res.InputTokens = firstNonZero(u.InputTokens, u.InputTokensCamel)
	res.OutputTokens = firstNonZero(u.OutputTokens, u.OutputTokensCamel)
	res.CacheReadTokens = firstNonZero(u.CacheReadInputTokens, u.CacheReadTokens)
	res.CacheWriteTokens = firstNonZero(u.CacheCreationInputTokens, u.CacheWriteTokens)
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// toolCalls collects tool uses and their results from the event stream.
type toolCalls struct {
	calls []store.ToolCall
	byID  map[string]int
}

type contentEvent struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func (tc *toolCalls) add(line []byte) {
	var typed struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &typed) != nil {
		return
	}
	switch typed.Type {
	case "assistant", "user":
		tc.addClaude(line)
	case "tool_call":
		tc.addCursor(line)
	}
}

func (tc *toolCalls) addClaude(line []byte) {
	var ev contentEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	var blocks []block
	if json.Unmarshal(ev.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			tc.byID[b.ID] = len(tc.calls)
			tc.calls = append(tc.calls, store.ToolCall{Seq: len(tc.calls) + 1, ToolUseID: b.ID, Name: b.Name, InputSummary: summarize(b.Input)})
		case "tool_result":
			i, ok := tc.byID[b.ToolUseID]
			if !ok || !b.IsError {
				continue
			}
			c := &tc.calls[i]
			c.IsError = true
			text := resultText(b.Content)
			switch {
			case strings.Contains(text, "PreToolUse") || strings.Contains(text, "taskpilot gate") || strings.Contains(text, "taskpilot policy"):
				c.DeniedBy, c.DenyReason = "hook", clip(text, 400)
			case strings.Contains(text, "Permission to use") && strings.Contains(text, "denied"):
				c.DeniedBy, c.DenyReason = "permission", clip(text, 400)
			}
		}
	}
}

func (tc *toolCalls) addCursor(line []byte) {
	var ev struct {
		Subtype  string                     `json:"subtype"`
		CallID   string                     `json:"call_id"`
		ToolCall map[string]json.RawMessage `json:"tool_call"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.CallID == "" {
		return
	}
	switch ev.Subtype {
	case "started":
		name, input := cursorTool(ev.ToolCall)
		tc.byID[ev.CallID] = len(tc.calls)
		tc.calls = append(tc.calls, store.ToolCall{Seq: len(tc.calls) + 1, ToolUseID: ev.CallID, Name: name, InputSummary: summarize(input)})
	case "completed":
		i, ok := tc.byID[ev.CallID]
		if !ok {
			return
		}
		if reason := cursorDenial(ev.ToolCall); reason != "" {
			c := &tc.calls[i]
			c.IsError = true
			c.DeniedBy, c.DenyReason = "hook", clip(reason, 400)
		}
	}
}

func cursorTool(m map[string]json.RawMessage) (name string, input json.RawMessage) {
	for k, raw := range m {
		switch k {
		case "readToolCall":
			return "Read", cursorArgs(raw)
		case "writeToolCall":
			return "Write", cursorArgs(raw)
		case "function":
			var f struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(raw, &f)
			if len(f.Arguments) > 0 && f.Arguments[0] == '"' {
				var s string
				if json.Unmarshal(f.Arguments, &s) == nil {
					f.Arguments = json.RawMessage(s)
				}
			}
			return f.Name, f.Arguments
		default:
			if strings.HasSuffix(k, "ToolCall") {
				return strings.TrimSuffix(k, "ToolCall"), cursorArgs(raw)
			}
		}
	}
	return "unknown", nil
}

func cursorArgs(raw json.RawMessage) json.RawMessage {
	var wrap struct {
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(raw, &wrap) == nil && len(wrap.Args) > 0 {
		return wrap.Args
	}
	return raw
}

func cursorDenial(m map[string]json.RawMessage) string {
	b, _ := json.Marshal(m)
	s := string(b)
	for _, needle := range []string{"taskpilot gate", "taskpilot policy", "permission\":\"deny", "Hook blocked"} {
		if strings.Contains(s, needle) {
			return clip(s, 400)
		}
	}
	return ""
}

var secretish = regexp.MustCompile(`eyJ[\w-]{8,}\.[\w-]{8,}\.[\w-]{4,}|(?i)(bearer|token|password|secret|cookie)([=: ]+)\S+|[A-Za-z0-9+/_-]{40,}`)

// summarize keeps the identifying part of a tool input, redacted and clipped.
func summarize(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	// graphify: "affected bom-report::…" (the action is what the stats count).
	if a, ok := m["action"].(string); ok && a != "" {
		t, _ := m["text"].(string)
		return clip(strings.TrimSpace(a+" "+t), 300)
	}
	for _, k := range []string{"command", "file_path", "pattern", "path", "query", "gate", "skill", "name", "app", "repo", "url"} {
		if v, ok := m[k].(string); ok && v != "" {
			return clip(secretish.ReplaceAllStringFunc(v, func(s string) string {
				if sub := secretish.FindStringSubmatch(s); len(sub) > 2 && sub[1] != "" {
					return sub[1] + sub[2] + "[redacted]"
				}
				return "[redacted]"
			}), 300)
		}
	}
	return ""
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

func clip(s string, n int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > n {
		return string(r[:n]) + "…"
	}
	return strings.TrimSpace(s)
}
