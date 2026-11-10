// Package runner launches one headless Claude Code turn for a task and
// records its stream-json events.
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
}

// systemContract is appended to Claude Code's system prompt for every run.
const systemContract = `You are an autonomous engineer running headless under taskpilot. No human reads this terminal.
Follow the taskpilot:workflow skill for the whole task. Talk to the operator only through the taskpilot MCP tools
(request_approval, complete_task) and a short final status line per turn. Write operator-facing text in %s.`

// Run executes one turn: a fresh session with the initial prompt, or a resume
// of t.SessionID with prompt. Stdout events are appended to the task's event log.
func (r *Runner) Run(ctx context.Context, t *store.Task, prompt string, resume bool) (*Result, error) {
	proj, ok := r.cfg().Projects[t.Project]
	if !ok {
		return nil, fmt.Errorf("unknown project %q", t.Project)
	}
	args, err := r.Args(t, proj, resume)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, r.cfg().Claude.Bin, args...)
	cmd.Dir = t.Workspace
	// Own process group: on stop, SIGTERM reaches claude and everything it
	// started (Bash tool commands, MCP servers) so nothing is orphaned and
	// claude can save the session; after WaitDelay the pipes are closed anyway.
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
	stderr, err := os.Create(r.Store.StderrPath(t.ID, t.Runs))
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
		return nil, fmt.Errorf("start %s: %w", r.cfg().Claude.Bin, err)
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
	if waitErr != nil {
		return res, fmt.Errorf("claude exited: %w (see %s)", waitErr, r.Store.StderrPath(t.ID, t.Runs))
	}
	if scanErr != nil {
		return res, fmt.Errorf("read claude output: %w", scanErr)
	}
	return res, nil
}

// Profile is the model and effort the task's next run uses: the task's own
// profile (picked by the agent or pinned by the operator) over the config
// default, field by field.
func (r *Runner) Profile(t *store.Task) (model, effort string) {
	c := r.cfg().Claude
	model, effort = c.Model, c.Effort
	if r.Store == nil {
		return model, effort
	}
	if p, err := r.Store.TaskProfile(t.ID); err == nil && p != nil {
		if p.Model != "" {
			model = p.Model
		}
		if p.Effort != "" {
			effort = p.Effort
		}
	}
	return model, effort
}

// Args builds the claude command line. The prompt is passed on stdin, so no
// positional argument can be swallowed by a variadic flag.
func (r *Runner) Args(t *store.Task, proj config.Project, resume bool) ([]string, error) {
	c := r.cfg().Claude
	mcpCfg, err := r.mcpConfig(t, proj)
	if err != nil {
		return nil, err
	}
	settings, err := r.settings(t)
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
		"--append-system-prompt", fmt.Sprintf(systemContract, r.cfg().OperatorLanguage),
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
	} else {
		args = append(args, "--session-id", t.SessionID)
	}
	// Last, because it is variadic.
	args = append(args, "--allowedTools", strings.Join(proj.AllowedToolList(), ","))
	return args, nil
}

func (r *Runner) mcpConfig(t *store.Task, proj config.Project) (string, error) {
	servers := map[string]config.MCPServer{
		"taskpilot": {
			Command: r.Exe,
			Args:    []string{"mcp", "--config", r.cfg().Path, "--task", t.ID},
		},
	}
	for name, s := range proj.MCPServers {
		servers[name] = s
	}
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	return string(b), err
}

func (r *Runner) settings(t *store.Task) (string, error) {
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// streamEvent is the subset of stream-json events the runner reads.
type streamEvent struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	SessionID    string  `json:"session_id"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	NumTurns     int     `json:"num_turns"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func parseEvent(line []byte, res *Result) {
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
		res.InputTokens, res.OutputTokens = ev.Usage.InputTokens, ev.Usage.OutputTokens
		res.CacheReadTokens, res.CacheWriteTokens = ev.Usage.CacheReadInputTokens, ev.Usage.CacheCreationInputTokens
	}
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
	var ev contentEvent
	if json.Unmarshal(line, &ev) != nil || (ev.Type != "assistant" && ev.Type != "user") {
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
			case strings.Contains(text, "PreToolUse"):
				c.DeniedBy, c.DenyReason = "hook", clip(text, 400)
			case strings.Contains(text, "Permission to use") && strings.Contains(text, "denied"):
				c.DeniedBy, c.DenyReason = "permission", clip(text, 400)
			}
		}
	}
}

var secretish = regexp.MustCompile(`eyJ[\w-]{8,}\.[\w-]{8,}\.[\w-]{4,}|(?i)(bearer|token|password|secret|cookie)([=: ]+)\S+|[A-Za-z0-9+/_-]{40,}`)

// summarize keeps the identifying part of a tool input, redacted and clipped.
func summarize(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
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
