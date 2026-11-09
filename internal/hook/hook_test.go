package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/gate"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func run(t *testing.T, st *store.Store, task, payload string) (denied bool, reason string) {
	t.Helper()
	pol, err := gate.Compile(config.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(strings.NewReader(payload), &out, st, task, pol); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return false, ""
	}
	var o output
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("hook output is not JSON: %q", out.String())
	}
	if o.HookSpecificOutput.HookEventName != "PreToolUse" || o.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("unexpected output %+v", o)
	}
	return true, o.HookSpecificOutput.PermissionDecisionReason
}

func TestHookEnforcesPlanGate(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	task := &store.Task{ID: "mock-1", Workspace: ws}
	task.Transition(store.StateRunning, "")
	if err := st.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	edit := `{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{"file_path":"svc/a.go"},"cwd":` + strconv.Quote(ws) + `}`

	if denied, reason := run(t, st, "mock-1", edit); !denied || !strings.Contains(reason, "request_approval") {
		t.Fatalf("edit before plan approval: denied=%v reason=%q", denied, reason)
	}
	req, err := st.AddRequest("mock-1", "plan", "s", "")
	if err != nil {
		t.Fatal(err)
	}
	if denied, _ := run(t, st, "mock-1", edit); !denied {
		t.Fatal("a pending (undecided) request must not open the gate")
	}
	if err := st.Decide("mock-1", req, true, ""); err != nil {
		t.Fatal(err)
	}
	if denied, reason := run(t, st, "mock-1", edit); denied {
		t.Fatalf("edit after approval denied: %s", reason)
	}
	if denied, _ := run(t, st, "mock-1", `{"tool_name":"Read","tool_input":{}}`); denied {
		t.Fatal("read must be allowed")
	}

	// Even with every gate approved, writes outside the workspace are refused.
	outside := `{"tool_name":"Write","tool_input":{"file_path":"/home/u/repos/bitbucket/BOM/bom-api/main.go"},"cwd":` + strconv.Quote(ws) + `}`
	if denied, reason := run(t, st, "mock-1", outside); !denied || !strings.Contains(reason, "inside the task workspace") {
		t.Fatalf("write outside workspace: denied=%v reason=%q", denied, reason)
	}
	escape := `{"tool_name":"Edit","tool_input":{"file_path":"../../x.go"},"cwd":` + strconv.Quote(ws+"/svc") + `}`
	if denied, _ := run(t, st, "mock-1", escape); !denied {
		t.Fatal("relative path escaping the workspace must be denied")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	viaLink := `{"tool_name":"Write","tool_input":{"file_path":` + strconv.Quote(filepath.Join(ws, "link", "x.go")) + `}}`
	if denied, _ := run(t, st, "mock-1", viaLink); !denied {
		t.Fatal("symlink pointing out of the workspace must be denied")
	}
}

func TestHookFailsClosed(t *testing.T) {
	st, _ := store.New(t.TempDir())
	if denied, _ := run(t, st, "nope", `{"tool_name":"Read"}`); !denied {
		t.Fatal("unknown task must deny")
	}
	if denied, _ := run(t, st, "../x", `{"tool_name":"Read"}`); !denied {
		t.Fatal("path-like task id must deny")
	}
	if denied, _ := run(t, st, "nope", `not json`); !denied {
		t.Fatal("garbage input must deny")
	}
}
