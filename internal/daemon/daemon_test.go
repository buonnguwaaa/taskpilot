package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/runner"
	"github.com/buonnguwaaa/taskpilot/internal/source"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

type harness struct {
	t   *testing.T
	d   *Daemon
	st  *store.Store
	src *source.Mock
	dir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	mustGit(t, "", "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")

	fake, err := filepath.Abs("testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf(`home: %s
claude: {bin: %s}
daemon: {concurrency: 2}
source: {type: mock, dir: %s}
projects:
  demo:
    name: Demo
    repos:
      api: {path: %s, base: main, verify: "true"}
`, filepath.Join(dir, "home"), fake, filepath.Join(dir, "inbox"), repo)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	src, err := source.NewMock(cfg.Source.Dir)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		Config: cfg, Store: st, Source: src,
		Runner: &runner.Runner{Config: cfg, Store: st, PluginDir: filepath.Join(dir, "plugin"), Exe: "/bin/true"},
		Log:    log.New(io.Discard, "", 0),
	}
	return &harness{t: t, d: d, st: st, src: src, dir: dir}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// step runs one scheduling pass and waits for the runs it started.
func (h *harness) step() {
	h.d.Tick(context.Background())
	h.d.Wait()
}

func (h *harness) task(id string) *store.Task {
	h.t.Helper()
	tk, err := h.st.LoadTask(id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

func (h *harness) expect(id string, state store.State, pendingID string) *store.Task {
	h.t.Helper()
	tk := h.task(id)
	if tk.State != state {
		h.t.Fatalf("state = %s (%s), want %s; history %+v", tk.State, tk.LastError, state, tk.History)
	}
	p, err := h.st.PendingRequest(id)
	if err != nil {
		h.t.Fatal(err)
	}
	got := ""
	if p != nil {
		got = p.ID
	}
	if got != pendingID {
		h.t.Fatalf("pending = %q, want %q", got, pendingID)
	}
	return tk
}

func (h *harness) decide(id string, approve bool, comment string) {
	h.t.Helper()
	p, err := h.st.PendingRequest(id)
	if err != nil || p == nil {
		h.t.Fatalf("no pending request: %v", err)
	}
	if err := h.st.Decide(id, p, approve, comment); err != nil {
		h.t.Fatal(err)
	}
}

func TestHappyPathThroughBothGates(t *testing.T) {
	h := newHarness(t)
	if err := h.src.Submit(source.Ticket{ID: "T1", Title: "fix it", Body: "AC: works", Project: "demo", Repo: "api"}); err != nil {
		t.Fatal(err)
	}
	const id = "mock-T1"

	h.step()
	tk := h.expect(id, store.StateAwaitApproval, "plan-1")
	if tk.Branch != "tp/"+id {
		t.Fatalf("branch = %q", tk.Branch)
	}
	if _, err := os.Stat(filepath.Join(tk.Workspace, "api", "README.md")); err != nil {
		t.Fatalf("hinted repo not opened in the workspace: %v", err)
	}
	if b, _ := h.st.Baselines(id); b["api"] == nil || !b["api"].Passed {
		t.Fatalf("baseline not recorded for the pre-opened repo: %+v", b)
	}

	// Nothing changes until the operator decides.
	h.step()
	h.expect(id, store.StateAwaitApproval, "plan-1")

	h.decide(id, true, "")
	h.step()
	h.expect(id, store.StateAwaitApproval, "pr-1")

	h.decide(id, true, "ship it")
	h.step()
	tk = h.expect(id, store.StateDone, "")
	if tk.Runs != 3 || tk.CostUSD < 0.74 || tk.CostUSD > 0.76 {
		t.Fatalf("runs=%d cost=%.2f, want 3 and 0.75", tk.Runs, tk.CostUSD)
	}

	b, err := os.ReadFile(filepath.Join(h.st.TaskDir(id), "fake-args.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d runs in args log", len(lines))
	}
	if !strings.Contains(lines[0], "--session-id "+tk.SessionID) {
		t.Errorf("first run should start the fixed session: %s", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "--resume "+tk.SessionID) {
			t.Errorf("later runs should resume the same session: %s", l)
		}
	}

	runs, err := h.st.Runs(id)
	if err != nil || len(runs) != 3 || runs[0].Trigger != "initial" || runs[1].Trigger != "approval" ||
		runs[2].Outcome != "done" || runs[0].ToolCalls != 2 || runs[0].Denied != 1 {
		t.Fatalf("runs = %+v %v", runs, err)
	}
	if st, _ := h.st.Stats(); st.OutboxPending != 0 || st.Runs7d != 3 {
		t.Fatalf("stats = %+v", st)
	}
	report, err := os.ReadFile(filepath.Join(h.src.Dir, "T1.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"picked up", "plan-1", "pr-1", "https://example/pr/1"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("ticket report missing %q:\n%s", want, report)
		}
	}
}

func TestRejectedPlanIsRevised(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "T2", Title: "x", Project: "demo"})
	const id = "mock-T2"
	h.step()
	h.expect(id, store.StateAwaitApproval, "plan-1")
	h.decide(id, false, "too broad")
	h.step()
	tk := h.expect(id, store.StateAwaitApproval, "plan-2")
	if approved, _ := h.st.GateApproved(id, "plan"); approved {
		t.Fatal("plan must not count as approved after a rejection")
	}
	_ = tk
}

func TestOperatorMessageResumesStoppedTask(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "T3", Title: "x", Project: "demo"})
	const id = "mock-T3"
	h.step()
	h.decide(id, true, "")
	h.step()
	h.decide(id, true, "")
	h.step()
	h.expect(id, store.StateDone, "")

	if err := h.st.SetNudge(id, "also update the changelog"); err != nil {
		t.Fatal(err)
	}
	h.step()
	// The fake agent ends this turn without a gate or a new completion; the
	// earlier completion must not make the follow-up look done.
	tk := h.expect(id, store.StateNeedsAttention, "")
	if tk.Runs != 4 {
		t.Fatalf("message should trigger a 4th run, got %d", tk.Runs)
	}
	if n, _ := h.st.TakeNudge(id); n != nil {
		t.Fatal("nudge should have been consumed")
	}
}

func TestUnknownProjectOrRepoFails(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "T4", Title: "x", Project: "nope"})
	h.src.Submit(source.Ticket{ID: "T7", Title: "x", Project: "demo", Repo: "other-team-repo"})
	h.step()
	if tk := h.expect("mock-T4", store.StateFailed, ""); !strings.Contains(tk.LastError, "unknown project") {
		t.Fatalf("LastError = %q", tk.LastError)
	}
	if tk := h.expect("mock-T7", store.StateFailed, ""); !strings.Contains(tk.LastError, "not part of project") {
		t.Fatalf("LastError = %q", tk.LastError)
	}
}

func TestWorkspaceWithoutHintStartsEmpty(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "T8", Title: "x", Project: "demo"})
	h.step()
	tk := h.expect("mock-T8", store.StateAwaitApproval, "plan-1")
	entries, err := os.ReadDir(tk.Workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace should exist and be empty until the agent opens repos: %v %v", entries, err)
	}
	b, _ := os.ReadFile(filepath.Join(h.st.TaskDir("mock-T8"), "fake-args.log"))
	if !strings.Contains(string(b), "--allowedTools") {
		t.Fatal("run did not happen")
	}
}

func TestTakeoverBlocksScheduling(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "T5", Title: "x", Project: "demo"})
	const id = "mock-T5"
	h.step()
	h.decide(id, true, "")
	if err := h.st.SetTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	h.step()
	h.expect(id, store.StateAwaitApproval, "") // decided, but parked while taken over
	h.st.SetTakeover(id, false)
	h.step()
	h.expect(id, store.StateAwaitApproval, "pr-1")
}

func TestRecoverParksInterruptedTasks(t *testing.T) {
	h := newHarness(t)
	tk := &store.Task{ID: "mock-T6", Project: "demo", SessionID: newUUID()}
	tk.Transition(store.StateRunning, "")
	if err := h.st.SaveTask(tk); err != nil {
		t.Fatal(err)
	}
	if err := h.d.recover(); err != nil {
		t.Fatal(err)
	}
	h.expect("mock-T6", store.StateNeedsAttention, "")
}

// stopDuringRun starts the task's run and stops taskpilot while it is running.
func (h *harness) stopDuringRun(id string) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.d.Tick(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if tk, err := h.st.LoadTask(id); err == nil && tk.State == store.StateRunning {
			if _, err := os.Stat(filepath.Join(h.st.TaskDir(id), "fake-args.log")); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatal("run never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	h.d.Wait()
	if time.Since(start) > 8*time.Second {
		h.t.Fatalf("stopping took %s: child processes kept the run alive", time.Since(start))
	}
}

func TestStopDuringRunThenResumeWithMessage(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "S1", Title: "SLOW task", Project: "demo"})
	h.stopDuringRun("mock-S1")
	tk := h.expect("mock-S1", store.StateNeedsAttention, "")
	if !strings.Contains(tk.History[len(tk.History)-1].Note, "interrupted") {
		t.Fatalf("note = %q", tk.History[len(tk.History)-1].Note)
	}
	// A message resumes the same session.
	h.st.SetNudge("mock-S1", "tiếp tục nhé")
	h.step()
	b, _ := os.ReadFile(filepath.Join(h.st.TaskDir("mock-S1"), "fake-args.log"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], "--resume "+tk.SessionID) {
		t.Fatalf("resume args = %q", lines)
	}
}

func TestStopRightAfterTheAgentReachedAGate(t *testing.T) {
	h := newHarness(t)
	h.src.Submit(source.Ticket{ID: "S2", Title: "SLOW-AFTER-GATE", Project: "demo"})
	h.stopDuringRun("mock-S2")
	// The request was recorded before the stop: the task waits at the gate, so approving it resumes it.
	h.expect("mock-S2", store.StateAwaitApproval, "plan-1")
	h.decide("mock-S2", true, "")
	h.step()
	h.expect("mock-S2", store.StateAwaitApproval, "pr-1")
}

func TestRecoverParksAtGateOrDone(t *testing.T) {
	h := newHarness(t)
	gate := &store.Task{ID: "mock-R1", Project: "demo", SessionID: newUUID()}
	gate.Transition(store.StateRunning, "")
	h.st.SaveTask(gate)
	h.st.AddRequest("mock-R1", "pr", "s", "")
	done := &store.Task{ID: "mock-R2", Project: "demo", SessionID: newUUID()}
	done.Transition(store.StateRunning, "")
	h.st.SaveTask(done)
	h.st.SetCompletion("mock-R2", &store.Completion{Summary: "xong"})
	if err := h.d.recover(); err != nil {
		t.Fatal(err)
	}
	h.expect("mock-R1", store.StateAwaitApproval, "pr-1")
	h.expect("mock-R2", store.StateDone, "")
}
