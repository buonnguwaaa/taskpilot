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
	if err := Run(strings.NewReader(payload), &out, st, task, pol, FormatClaude); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return false, ""
	}
	var o claudeOutput
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

// AskUserQuestion cannot be answered headless: the hook turns it into a
// question request for the operator and denies the call.
func TestHookForwardsAskUserQuestion(t *testing.T) {
	st, _ := store.New(t.TempDir())
	tk := &store.Task{ID: "mock-q", Workspace: t.TempDir()}
	tk.Transition(store.StateRunning, "")
	st.SaveTask(tk)
	payload := `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[` +
		`{"question":"Lưu thông tin trả góp ở đâu?","header":"Lưu trữ","multiSelect":false,"options":[` +
		`{"label":"Bảng mới installment_infos","description":"(khuyến nghị) tách bạch"},{"label":"Cột JSON trong transactions"}]}]}}`
	denied, reason := run(t, st, "mock-q", payload)
	if !denied || !strings.Contains(reason, "question-1") || !strings.Contains(reason, "end your turn") {
		t.Fatalf("denied=%v reason=%q", denied, reason)
	}
	p, _ := st.PendingRequest("mock-q")
	if p == nil || p.Gate != store.QuestionGate || len(p.Questions) != 1 || p.Questions[0].Options[0].Label != "Bảng mới installment_infos" || p.Questions[0].Header != "Lưu trữ" {
		t.Fatalf("pending = %+v", p)
	}
	// A second question while one is pending is refused, not duplicated.
	_, reason = run(t, st, "mock-q", payload)
	if !strings.Contains(reason, "still waiting") {
		t.Fatalf("second ask: %q", reason)
	}
	// Bad input (one option) is reported back without a request.
	st2, _ := store.New(t.TempDir())
	tk2 := &store.Task{ID: "mock-b"}
	tk2.Transition(store.StateRunning, "")
	st2.SaveTask(tk2)
	_, reason = run(t, st2, "mock-b", `{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"x","options":[{"label":"a"}]}]}}`)
	if !strings.Contains(reason, "not sent") {
		t.Fatalf("invalid: %q", reason)
	}
	if p, _ := st2.PendingRequest("mock-b"); p != nil {
		t.Fatal("no request for invalid questions")
	}
}

func TestCursorHookFormat(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	task := &store.Task{ID: "mock-c", Workspace: ws}
	task.Transition(store.StateRunning, "")
	if err := st.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	pol, err := gate.Compile(config.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	write := `{"hook_event_name":"preToolUse","tool_name":"Write","tool_input":{"path":"a.go"},"cwd":` + strconv.Quote(ws) + `}`
	var out bytes.Buffer
	if err := Run(strings.NewReader(write), &out, st, "mock-c", pol, ""); err != nil {
		t.Fatal(err)
	}
	var o cursorOutput
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("output: %q", out.String())
	}
	if o.Permission != "deny" || !strings.Contains(o.AgentMessage, "request_approval") {
		t.Fatalf("%+v", o)
	}
	req, err := st.AddRequest("mock-c", "plan", "s", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Decide("mock-c", req, true, ""); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(strings.NewReader(write), &out, st, "mock-c", pol, ""); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &o); err != nil {
		t.Fatalf("allow output: %q", out.String())
	}
	if o.Permission != "allow" {
		t.Fatalf("want allow, got %+v", o)
	}
}

func TestGraphifyReminderOncePerRun(t *testing.T) {
	st, _ := store.New(t.TempDir())
	task := &store.Task{ID: "mock-g", Workspace: t.TempDir(), Runs: 1}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	pol, _ := gate.Compile(config.DefaultRules())
	call := func(payload string, graphify bool) (bool, string) {
		var out bytes.Buffer
		if err := RunWith(strings.NewReader(payload), &out, st, "mock-g", pol, FormatClaude, Options{Graphify: graphify}); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			return false, ""
		}
		var o claudeOutput
		json.Unmarshal(out.Bytes(), &o)
		return true, o.HookSpecificOutput.PermissionDecisionReason
	}
	grep := `{"hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"GenerateWorkOrderV2"}}`
	rg := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cd bom-api && rg -n QueryPaymentStatus"}}`
	build := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`

	if denied, _ := call(grep, false); denied {
		t.Fatal("no graphs: no reminder")
	}
	if denied, _ := call(build, true); denied {
		t.Fatal("a build is not a search")
	}
	denied, reason := call(rg, true)
	if !denied || !strings.Contains(reason, `ToolSearch "select:mcp__taskpilot__graphify"`) || !strings.Contains(reason, "next search is allowed") {
		t.Fatalf("first search of the run: denied=%v %q", denied, reason)
	}
	if denied, _ := call(grep, true); denied {
		t.Fatal("reminded once per run only")
	}
	// Next run: reminded again, unless the agent queries the graph first.
	task.Runs = 2
	st.SaveTask(task)
	if denied, _ := call(grep, true); !denied {
		t.Fatal("a new run gets the reminder")
	}
	task.Runs = 3
	st.SaveTask(task)
	st.MarkGraphifyUsed("mock-g")
	if denied, _ := call(grep, true); denied {
		t.Fatal("no reminder once the graph was queried in the run")
	}
}
