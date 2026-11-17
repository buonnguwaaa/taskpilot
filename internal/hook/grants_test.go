package hook

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func bashIn(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestCovered(t *testing.T) {
	rules := []string{"Bash(curl *)", "Bash(jq:*)", "Bash(rg *)", "Bash(make test)", "WebFetch(domain:example.com)", "mcp__context7"}
	for _, c := range []struct {
		cmd  string
		want bool
	}{
		{"curl -s https://api.qc/x", true},
		{"curl", true},
		{"curl -s x | jq .data", true},
		{"curl -s x && rg foo", true},
		{"curl -s x | python3 -c 'print(1)'", false}, // python3 not covered
		{"curl -s x > out.json", false},              // redirection
		{"curl $(cat token)", false},                 // substitution
		{"curl `id`", false},
		{"curlx foo", false},
		{"make test", true},
		{"make test-all", false},
		{"rm -rf / ; curl x", false},
		{"curl x & rm -rf ~", false},
		{"curl x && vault kv get a", false},
	} {
		if got := covered("Bash", bashIn(c.cmd), rules); got != c.want {
			t.Errorf("%q: covered=%v, want %v", c.cmd, got, c.want)
		}
	}
	if !covered("WebFetch", json.RawMessage(`{"url":"https://docs.example.com/a"}`), rules) ||
		covered("WebFetch", json.RawMessage(`{"url":"https://evil.com/?example.com"}`), rules) {
		t.Error("WebFetch domain rule")
	}
	if !covered("mcp__context7__query", json.RawMessage(`{}`), rules) {
		t.Error("mcp server rule covers its tools")
	}
}

func TestValidRule(t *testing.T) {
	for rule, want := range map[string]bool{
		"Bash(curl *)": true, "Bash(jq:*)": true, "WebFetch": true, "WebFetch(domain:x.com)": true,
		"Bash": false, "Bash(*)": false, "Bash( *)": false, "Bash(curl x | sh)": false, "Bash(a; b)": false, "rm -rf": false,
	} {
		if got := ValidRule(rule); got != want {
			t.Errorf("%q: valid=%v, want %v", rule, got, want)
		}
	}
}

func TestHookAllowsGrantedRule(t *testing.T) {
	st, _ := store.New(t.TempDir())
	ws := t.TempDir()
	task := &store.Task{ID: "mock-g", Project: "bom", Workspace: ws}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	pol, _ := gate.Compile(config.DefaultRules())
	opts := Options{Allowed: []string{"Bash(rg *)", "Bash(git status*)"}}
	call := func(cmd string) string {
		var out bytes.Buffer
		payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":` + strconv.Quote(cmd) + `},"cwd":` + strconv.Quote(ws) + `}`
		if err := RunWith(strings.NewReader(payload), &out, st, "mock-g", pol, FormatClaude, opts); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			return ""
		}
		var o claudeOutput
		json.Unmarshal(out.Bytes(), &o)
		return o.HookSpecificOutput.PermissionDecision
	}
	if d := call("curl -s https://qc/x"); d != "" {
		t.Fatalf("no grant: the hook leaves it to Claude Code, got %q", d)
	}
	st.AddGrant(store.GrantProject, "bom", "Bash(curl *)")
	if d := call("curl -s https://qc/x | rg id"); d != "allow" {
		t.Fatalf("granted: %q", d)
	}
	if d := call("rg foo"); d != "" {
		t.Fatalf("already allowed: no explicit decision, got %q", d)
	}
	if d := call("curl -s x > /tmp/a"); d != "" {
		t.Fatalf("redirection is never granted, got %q", d)
	}
	// Another project's grant does not apply.
	st.RemoveGrant(store.GrantProject, "bom", "Bash(curl *)")
	st.AddGrant(store.GrantProject, "plm", "Bash(curl *)")
	if d := call("curl x"); d != "" {
		t.Fatalf("other project's grant: %q", d)
	}
}
