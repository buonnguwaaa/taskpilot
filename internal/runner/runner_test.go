package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestArgs(t *testing.T) {
	cfg := &config.Config{
		Path:             "/etc/tp.yaml",
		OperatorLanguage: "Vietnamese",
		Claude:           config.Claude{Bin: "claude", PermissionMode: "dontAsk", MaxBudgetUSD: 1.5, Model: "sonnet"},
	}
	repo := config.Project{
		MCPServers: map[string]config.MCPServer{"graphify": {Command: "python3", Args: []string{"-m", "graphify.serve"}}},
		Repos: map[string]config.Repo{
			"api": {AllowedTools: []string{"Bash(go test *)"}},
			"web": {AllowedTools: []string{"Bash(npm run *)", "Bash(go test *)"}},
		},
	}
	r := &Runner{Config: cfg, PluginDir: "/p", Exe: "/bin/tp it's"}
	task := &store.Task{ID: "mock-1", SessionID: "11111111-1111-4111-8111-111111111111"}

	args, err := r.Args(task, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-p" || argValue(args, "--session-id") != task.SessionID || argValue(args, "--resume") != "" {
		t.Fatalf("fresh run args: %v", args)
	}
	if argValue(args, "--max-budget-usd") != "1.50" || argValue(args, "--model") != "sonnet" || argValue(args, "--permission-prompts") != "none" {
		t.Fatalf("args: %v", args)
	}
	if args[len(args)-2] != "--allowedTools" {
		t.Fatalf("variadic --allowedTools must be last: %v", args[len(args)-3:])
	}
	tools := args[len(args)-1]
	if strings.Count(tools, "Bash(go test *)") != 1 {
		t.Errorf("repo tool lists must be merged without duplicates: %s", tools)
	}
	for _, want := range []string{"mcp__taskpilot", "mcp__graphify", "Bash(go test *)", "Bash(npm run *)", "Edit"} {
		if !strings.Contains(tools, want) {
			t.Errorf("allowed tools missing %s: %s", want, tools)
		}
	}

	var mcpCfg struct {
		MCPServers map[string]config.MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(argValue(args, "--mcp-config")), &mcpCfg); err != nil {
		t.Fatal(err)
	}
	tp := mcpCfg.MCPServers["taskpilot"]
	if tp.Command != "/bin/tp it's" || strings.Join(tp.Args, " ") != "mcp --config /etc/tp.yaml --task mock-1" {
		t.Fatalf("taskpilot mcp server = %+v", tp)
	}
	if _, ok := mcpCfg.MCPServers["graphify"]; !ok {
		t.Fatal("repo MCP server missing")
	}

	settings := argValue(args, "--settings")
	if !strings.Contains(settings, `'/bin/tp it'\"'\"'s' hook --config '/etc/tp.yaml' --task 'mock-1'`) {
		t.Fatalf("hook command not shell-quoted: %s", settings)
	}

	args, _ = r.Args(task, repo, true)
	if argValue(args, "--resume") != task.SessionID || argValue(args, "--session-id") != "" {
		t.Fatalf("resume args: %v", args)
	}
	cfg.Claude.MaxBudgetUSD = 0
	if args, _ = r.Args(task, repo, false); strings.Contains(strings.Join(args, " "), "--max-budget-usd") {
		t.Fatalf("no budget set: no cap, got %v", args)
	}
}

func TestParseEvent(t *testing.T) {
	res := &Result{}
	parseEvent([]byte(`{"type":"system","subtype":"init","session_id":"abc"}`), res)
	parseEvent([]byte(`not json`), res)
	parseEvent([]byte(`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"","total_cost_usd":1.2,"session_id":"abc"}`), res)
	if res.SessionID != "abc" || !res.IsError || res.Subtype != "error_max_budget_usd" || res.CostUSD != 1.2 {
		t.Fatalf("%+v", res)
	}
}

func TestToolCallsFromStream(t *testing.T) {
	tc := &toolCalls{byID: map[string]int{}}
	tc.add([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"},
		{"type":"tool_use","id":"a","name":"Bash","input":{"command":"curl -H 'Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefgh' https://x"}},
		{"type":"tool_use","id":"b","name":"Write","input":{"file_path":"/repo/a.go"}},
		{"type":"tool_use","id":"c","name":"Bash","input":{"command":"cat > f"}}]}}`))
	tc.add([]byte(`{"type":"user","message":{"content":[
		{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"PreToolUse:Write hook error: taskpilot gate \"plan\" is not approved yet"},
		{"type":"tool_result","tool_use_id":"c","is_error":true,"content":[{"type":"text","text":"Permission to use Bash has been denied because Claude Code is running in don't ask mode."}]},
		{"type":"tool_result","tool_use_id":"a","content":"ok"}]}}`))
	if len(tc.calls) != 3 {
		t.Fatalf("calls = %+v", tc.calls)
	}
	if s := tc.calls[0].InputSummary; strings.Contains(s, "eyJ") || !strings.Contains(s, "[redacted]") {
		t.Fatalf("secret not redacted: %q", s)
	}
	if c := tc.calls[1]; !c.IsError || c.DeniedBy != "hook" || !strings.Contains(c.DenyReason, "gate") {
		t.Fatalf("hook denial = %+v", c)
	}
	if c := tc.calls[2]; c.DeniedBy != "permission" {
		t.Fatalf("permission denial = %+v", c)
	}
	if tc.calls[0].IsError {
		t.Fatal("successful call marked as error")
	}
}

func TestArgsUseTaskProfile(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "mock-p", SessionID: "11111111-1111-4111-8111-111111111111"}
	task.Transition(store.StateQueued, "")
	st.SaveTask(task)
	cfg := &config.Config{Claude: config.Claude{Bin: "claude", Model: "sonnet", Effort: "medium", Models: []string{"opus", "sonnet"}}}
	r := &Runner{Config: cfg, Store: st, PluginDir: "/p", Exe: "/bin/tp"}
	proj := config.Project{}

	args, _ := r.Args(task, proj, false)
	if argValue(args, "--model") != "sonnet" || argValue(args, "--effort") != "medium" {
		t.Fatalf("no profile: config default, got %v", args)
	}
	if !strings.Contains(args[len(args)-1], "Agent") {
		t.Fatal("subagents (Agent) must be allowed")
	}
	st.SetTaskProfile("mock-p", &store.Profile{Model: "opus", Effort: "xhigh", Reason: "cross-repo", SetBy: "agent"})
	args, _ = r.Args(task, proj, true)
	if argValue(args, "--model") != "opus" || argValue(args, "--effort") != "xhigh" {
		t.Fatalf("agent profile not applied: %v", args)
	}
	// The operator pins a model only: the effort falls back to the config.
	st.SetTaskProfile("mock-p", &store.Profile{Model: "sonnet", SetBy: "operator"})
	if m, e := r.Profile(task); m != "sonnet" || e != "medium" {
		t.Fatalf("pinned model only: %s/%s", m, e)
	}
	if err := st.SetTaskProfile("mock-p", &store.Profile{Model: "opus", Effort: "max", SetBy: "agent"}); err != store.ErrProfilePinned {
		t.Fatalf("the agent cannot override a pinned profile: %v", err)
	}
}
