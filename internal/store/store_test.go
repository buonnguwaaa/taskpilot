package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func saved(t *testing.T, s *Store, id string, st State) *Task {
	t.Helper()
	tk := &Task{ID: id, Title: "t", Project: "bom"}
	tk.Transition(st, "created")
	if err := s.SaveTask(tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTaskRoundTripAndHistory(t *testing.T) {
	s := newStore(t)
	tk := saved(t, s, "mock-1", StateQueued)
	tk.Transition(StateRunning, "run 1")
	tk.Runs, tk.CostUSD, tk.NextTrigger = 1, 0.5, "approval"
	if err := s.SaveTask(tk); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadTask("mock-1")
	if err != nil || got.State != StateRunning || got.Runs != 1 || got.CostUSD != 0.5 || got.NextTrigger != "approval" {
		t.Fatalf("loaded %+v %v", got, err)
	}
	if len(got.History) != 2 || got.History[1].Note != "run 1" {
		t.Fatalf("history = %+v (saving twice must not duplicate entries)", got.History)
	}
	got.Transition(StateDone, "ok")
	s.SaveTask(got)
	all, _ := s.ListTasks()
	if len(all) != 1 || len(all[0].History) != 3 {
		t.Fatalf("list = %+v", all)
	}
	if _, err := s.LoadTask("nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestApprovals(t *testing.T) {
	s := newStore(t)
	saved(t, s, "mock-1", StateRunning)
	r1, err := s.AddRequest("mock-1", "plan", "p", "d")
	if err != nil || r1.ID != "plan-1" {
		t.Fatalf("%+v %v", r1, err)
	}
	if _, err := s.AddRequest("mock-1", "pr", "x", ""); err == nil {
		t.Fatal("second pending request accepted")
	}
	if err := s.Decide("mock-1", r1, false, "thiếu test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Decide("mock-1", r1, true, ""); err == nil {
		t.Fatal("deciding twice must fail")
	}
	r2, _ := s.AddRequest("mock-1", "plan", "p2", "")
	if r2.ID != "plan-2" {
		t.Fatalf("attempt numbering: %s", r2.ID)
	}
	if p, _ := s.PendingRequest("mock-1"); p == nil || p.ID != "plan-2" {
		t.Fatalf("pending = %+v", p)
	}
	s.Decide("mock-1", r2, true, "")
	if ok, _ := s.GateApproved("mock-1", "plan"); !ok {
		t.Fatal("plan approved")
	}
	decs, _ := s.Decisions("mock-1")
	if len(decs) != 2 || decs["plan-1"].Approved || decs["plan-1"].Comment != "thiếu test" || !decs["plan-2"].Approved {
		t.Fatalf("decisions = %+v", decs)
	}
}

func TestCompletionNudgesBaselinesPRs(t *testing.T) {
	s := newStore(t)
	saved(t, s, "mock-1", StateRunning)
	s.SetCompletion("mock-1", &Completion{Summary: "xong", PRURL: "u"})
	if c, _ := s.Completion("mock-1"); c == nil || c.Summary != "xong" {
		t.Fatal("completion")
	}
	s.ArchiveCompletion("mock-1")
	if c, _ := s.Completion("mock-1"); c != nil {
		t.Fatal("archived completion must not count")
	}
	s.SetNudge("mock-1", "a")
	s.SetNudge("mock-1", "b")
	if n, _ := s.TakeNudge("mock-1"); n == nil || n.Message != "a\n\nb" {
		t.Fatalf("nudge = %+v", n)
	}
	if n, _ := s.TakeNudge("mock-1"); n != nil {
		t.Fatal("consumed")
	}
	s.SetBaseline("mock-1", &Baseline{Repo: "bom-api", Command: "go test", Passed: false, Output: "FAIL"})
	s.SetBaseline("mock-1", &Baseline{Repo: "bom-api", Command: "go test", Passed: true})
	if b, _ := s.Baselines("mock-1"); len(b) != 1 || !b["bom-api"].Passed {
		t.Fatalf("baselines = %+v", b)
	}
	s.SetPullRequest("mock-1", &PullRequest{Repo: "bom-api", Destination: "release/qc", URL: "u1", Pending: true})
	s.SetPullRequest("mock-1", &PullRequest{Repo: "bom-api", Destination: "release/qc", URL: "u2", ID: 5})
	s.SetPullRequest("mock-1", &PullRequest{Repo: "bom-api", Destination: "main", URL: "u3", ID: 6})
	prs, _ := s.PullRequests("mock-1")
	if len(prs) != 2 {
		t.Fatalf("prs = %+v", prs)
	}
	if p, _ := s.PullRequestInto("mock-1", "bom-api", "release/qc"); p == nil || p.ID != 5 || p.Pending {
		t.Fatalf("upsert = %+v", p)
	}
}

func TestRunsToolCallsAndStats(t *testing.T) {
	s := newStore(t)
	saved(t, s, "mock-1", StateRunning)
	id, err := s.StartRun("mock-1", RunStart{N: 1, SessionID: "s", Trigger: "initial", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	calls := []ToolCall{{Seq: 1, Name: "Edit"}, {Seq: 2, Name: "Bash", InputSummary: "git push", IsError: true, DeniedBy: "hook", DenyReason: "gate pr"}}
	if err := s.FinishRun(id, RunEnd{CostUSD: 0.4, NumTurns: 7, Outcome: "awaiting_approval"}, calls); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs("mock-1")
	if len(runs) != 1 || runs[0].ToolCalls != 2 || runs[0].Denied != 1 || runs[0].Outcome != "awaiting_approval" || runs[0].CostUSD != 0.4 {
		t.Fatalf("runs = %+v", runs)
	}
	r, _ := s.AddRequest("mock-1", "plan", "p", "")
	s.Decide("mock-1", r, true, "")
	st, err := s.Stats()
	if err != nil || st.CostToday != 0.4 || st.Runs7d != 1 || len(st.Gates) != 1 || st.Gates[0].ApprovalRate != 1 ||
		len(st.TopDenials) != 1 || !strings.Contains(st.TopDenials[0], "Bash (hook)") {
		t.Fatalf("stats = %+v %v", st, err)
	}
}

func TestOutboxRetries(t *testing.T) {
	s := newStore(t)
	saved(t, s, "mock-1", StateRunning)
	id, _ := s.Enqueue("mock-1", "hw", "HSK-1", "đang chờ duyệt")
	due, _ := s.DueOutbox(10)
	if len(due) != 1 || due[0].Message != "đang chờ duyệt" {
		t.Fatalf("due = %+v", due)
	}
	s.MarkFailed(id, errors.New("503"), 2)
	if due, _ := s.DueOutbox(10); len(due) != 0 {
		t.Fatal("a failed report waits for its backoff")
	}
	s.DB.Exec(`UPDATE outbox SET next_attempt_at = '2000-01-01T00:00:00Z'`)
	s.MarkFailed(id, errors.New("503"), 2)
	if st, _ := s.Stats(); st.OutboxFailed != 1 || st.OutboxPending != 0 {
		t.Fatalf("gives up after max attempts: %+v", st)
	}
	id2, _ := s.Enqueue("mock-1", "hw", "HSK-1", "xong")
	s.MarkSent(id2)
	if st, _ := s.Stats(); st.OutboxPending != 0 {
		t.Fatal("sent")
	}
}

// writeLegacyTask lays out a task exactly as the file-based store did.
func writeLegacyTask(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "tasks", "hw-HSK-9")
	w := func(rel string, v any) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		b, _ := json.Marshal(v)
		os.WriteFile(p, b, 0o644)
	}
	at := time.Date(2026, 11, 9, 10, 43, 32, 0, time.UTC)
	w("task.json", map[string]any{"id": "hw-HSK-9", "source": "hw", "ticket_id": "HSK-9", "title": "Sửa MO", "project": "bom",
		"branch": "tp/hw-HSK-9", "workspace": "/ws", "session_id": "sid", "state": "awaiting_approval", "runs": 1, "cost_usd": 0.58,
		"created_at": at, "updated_at": at.Add(2 * time.Minute),
		"history": []map[string]any{{"at": at, "state": "queued", "note": "picked up"}, {"at": at.Add(2 * time.Minute), "state": "awaiting_approval", "note": "gate plan"}}})
	w("requests/plan-1.json", map[string]any{"id": "plan-1", "gate": "plan", "summary": "s1", "created_at": at})
	w("decisions/plan-1.json", map[string]any{"request_id": "plan-1", "gate": "plan", "approved": false, "comment": "lại", "decided_at": at.Add(time.Minute)})
	w("requests/plan-2.json", map[string]any{"id": "plan-2", "gate": "plan", "summary": "s2", "details": "chi tiết", "created_at": at.Add(time.Minute)})
	w("baselines/bom-api.json", map[string]any{"repo": "bom-api", "command": "go test ./...", "passed": false, "output": "FAIL", "at": at})
	w("prs/bom-api__release_qc.json", map[string]any{"repo": "bom-api", "destination": "release/qc", "url": "u", "pending": true, "created_at": at})
	w("nudge.json", map[string]any{"message": "nhớ chạy test", "created_at": at})
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "splunk-queries.jsonl"), []byte(`{"at":"2026-11-09T10:50:00Z","query":"q","earliest":"-1h","events":3}`+"\n"), 0o644)
}

func TestImportLegacy(t *testing.T) {
	home := t.TempDir()
	writeLegacyTask(t, home)
	s, err := New(home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ids, err := s.ImportLegacy()
	if err != nil || len(ids) != 1 {
		t.Fatalf("import = %v %v", ids, err)
	}
	tk, err := s.LoadTask("hw-HSK-9")
	if err != nil || tk.State != StateAwaitApproval || tk.CostUSD != 0.58 || len(tk.History) != 2 || tk.SessionID != "sid" {
		t.Fatalf("task = %+v %v", tk, err)
	}
	if p, _ := s.PendingRequest("hw-HSK-9"); p == nil || p.ID != "plan-2" || p.Details != "chi tiết" {
		t.Fatalf("the pending plan must survive the import: %+v", p)
	}
	if d, _ := s.Decisions("hw-HSK-9"); d["plan-1"] == nil || d["plan-1"].Comment != "lại" {
		t.Fatalf("decisions = %+v", d)
	}
	if b, _ := s.Baselines("hw-HSK-9"); b["bom-api"] == nil {
		t.Fatal("baseline")
	}
	if p, _ := s.PullRequestInto("hw-HSK-9", "bom-api", "release/qc"); p == nil || !p.Pending {
		t.Fatal("pr")
	}
	if n, _ := s.TakeNudge("hw-HSK-9"); n == nil || n.Message != "nhớ chạy test" {
		t.Fatal("unconsumed nudge")
	}
	if q, _ := s.SplunkQueries("hw-HSK-9"); len(q) != 1 || q[0].Events != 3 {
		t.Fatal("splunk audit")
	}
	dir := filepath.Join(home, "tasks", "hw-HSK-9")
	if _, err := os.Stat(filepath.Join(dir, "legacy", "task.json")); err != nil {
		t.Fatal("legacy files must be kept under legacy/")
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
		t.Fatal("events.jsonl stays in place")
	}
	if again, err := s.ImportLegacy(); err != nil || len(again) != 0 {
		t.Fatalf("import must be idempotent: %v %v", again, err)
	}
}
