package gate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func bash(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func file(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"file_path": p})
	return b
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

var all = []string{"plan", "pr", "deploy"}

func TestDefaultRules(t *testing.T) {
	pol, err := Compile(config.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		call     Call
		approved []string
		allow    bool
		reason   string
	}{
		{"read is free", Call{Tool: "Read"}, nil, true, ""},
		{"edit before plan", Call{Tool: "Edit"}, nil, false, `gate "plan"`},
		{"edit after plan", Call{Tool: "Edit"}, []string{"plan"}, true, ""},
		{"tool regex is anchored", Call{Tool: "EditorThing"}, nil, true, ""},
		{"go test is free", Call{Tool: "Bash", Input: bash("go test ./...")}, nil, true, ""},
		{"push before pr", Call{Tool: "Bash", Input: bash("git push -u origin tp/x")}, []string{"plan"}, false, `gate "pr"`},
		{"chained push before pr", Call{Tool: "Bash", Input: bash("go test ./... && git push")}, []string{"plan"}, false, `gate "pr"`},
		{"push after pr", Call{Tool: "Bash", Input: bash("git push -u origin tp/x")}, []string{"plan", "pr"}, true, ""},
		{"force push never", Call{Tool: "Bash", Input: bash("git push --force origin x")}, []string{"plan", "pr"}, false, "force-push"},
		{"argocd sync before deploy", Call{Tool: "Bash", Input: bash("argocd app sync web-qc")}, []string{"plan", "pr"}, false, `gate "deploy"`},
		{"argocd get is free", Call{Tool: "Bash", Input: bash("argocd app get web-qc")}, nil, true, ""},
		{"argocd mcp sync before deploy", Call{Tool: "mcp__argocd__sync_application"}, nil, false, `gate "deploy"`},
		{"vault read never", Call{Tool: "Bash", Input: bash("vault kv get secret/app")}, []string{"plan", "pr", "deploy"}, false, "Vault"},
		{"vault list is free", Call{Tool: "Bash", Input: bash("vault kv list secret/")}, nil, true, ""},
		{"read db", Call{Tool: "Read", Input: file("/home/u/.taskpilot/taskpilot.db")}, all, false, "off-limits"},
		{"read db relative", Call{Tool: "Read", Input: file("../../taskpilot.db")}, all, false, "off-limits"},
		{"read config", Call{Tool: "Read", Input: file("/home/u/.config/taskpilot/config.yaml")}, all, false, "off-limits"},
		{"read task state", Call{Tool: "Read", Input: file("/home/u/.taskpilot/tasks/mock-1/task.json")}, all, false, "off-limits"},
		{"grep harness home", Call{Tool: "Grep", Input: raw(`{"pattern":"token","path":"/home/u/.taskpilot"}`)}, all, false, "off-limits"},
		{"glob harness home", Call{Tool: "Glob", Input: raw(`{"pattern":"/home/u/.taskpilot/*"}`)}, all, false, "off-limits"},
		{"bash sqlite db", Call{Tool: "Bash", Input: bash("sqlite3 ~/.taskpilot/taskpilot.db .dump")}, all, false, "off-limits"},
		{"bash rg harness", Call{Tool: "Bash", Input: bash("rg -a wshr ~/.taskpilot")}, all, false, "off-limits"},
		{"bash chrome profile", Call{Tool: "Bash", Input: bash("ls /mnt/c/Users/x/AppData/Local/taskpilot-chrome")}, all, false, "off-limits"},
		{"own worktree is fine", Call{Tool: "Read", Input: file("/home/u/.taskpilot/worktrees/mock-1/main.go")}, all, true, ""},
		{"edit in worktree is fine", Call{Tool: "Edit", Input: file("/home/u/.taskpilot/worktrees/mock-1/main.go")}, all, true, ""},
		{"test in worktree is fine", Call{Tool: "Bash", Input: bash("cd /home/u/.taskpilot/worktrees/mock-1 && go test ./...")}, all, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := pol.Evaluate(c.call, func(g string) (bool, error) {
				for _, a := range c.approved {
					if a == g {
						return true, nil
					}
				}
				return false, nil
			})
			if v.Allow != c.allow {
				t.Fatalf("allow = %v, want %v (reason %q)", v.Allow, c.allow, v.Reason)
			}
			if c.reason != "" && !strings.Contains(v.Reason, c.reason) {
				t.Fatalf("reason %q does not mention %q", v.Reason, c.reason)
			}
		})
	}
}

func TestApprovalErrorFailsClosed(t *testing.T) {
	pol, _ := Compile(config.DefaultRules())
	v := pol.Evaluate(Call{Tool: "Write"}, func(string) (bool, error) { return false, errors.New("disk gone") })
	if v.Allow {
		t.Fatal("an error while checking approvals must deny")
	}
}

func TestCompileRejectsBadRegex(t *testing.T) {
	if _, err := Compile([]config.Rule{{Tool: "(", Requires: "plan"}}); err == nil {
		t.Fatal("expected error")
	}
}
