package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/bitbucket"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func setup(t *testing.T) (*mcp.ClientSession, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(st.Home, "worktrees", "mock-1")
	os.MkdirAll(ws, 0o755)
	task := &store.Task{ID: "mock-1", Title: "fix login", Body: "AC: ...", Project: "pay", Branch: "tp/mock-1", Workspace: ws}
	task.Transition(store.StateRunning, "")
	if err := st.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Gates:            []string{"plan", "pr", "deploy"},
		OperatorLanguage: "Vietnamese",
		Projects: map[string]config.Project{"pay": {
			Name: "Payment",
			Repos: map[string]config.Repo{
				"web":    {Path: gitRepo(t), Base: "main", Remote: "-", Verify: "echo all good"},
				"legacy": {Path: gitRepo(t), Base: "main", Remote: "-", Verify: "echo vet: bad format; exit 1"},
			},
		}},
	}
	srv := NewServer(&Handlers{Store: st, Config: cfg, TaskID: "mock-1"})
	ct, srvT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, st
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestApprovalFlowOverMCP(t *testing.T) {
	cs, st := setup(t)

	if _, isErr := call(t, cs, "request_approval", map[string]any{"gate": "merge", "summary": "x"}); !isErr {
		t.Fatal("unknown gate must be an error")
	}
	text, isErr := call(t, cs, "request_approval", map[string]any{"gate": "plan", "summary": "đổi validate", "details": "1. ..."})
	if isErr || !strings.Contains(text, "plan-1") {
		t.Fatalf("request_approval: %q isErr=%v", text, isErr)
	}
	if _, isErr := call(t, cs, "request_approval", map[string]any{"gate": "pr", "summary": "x"}); !isErr {
		t.Fatal("a second request while one is pending must be an error")
	}
	if _, isErr := call(t, cs, "complete_task", map[string]any{"summary": "x"}); !isErr {
		t.Fatal("complete_task while a request is pending must be an error")
	}

	req, _ := st.PendingRequest("mock-1")
	if err := st.Decide("mock-1", req, false, "thiếu test"); err != nil {
		t.Fatal(err)
	}
	info, isErr := call(t, cs, "task_info", nil)
	if isErr {
		t.Fatalf("task_info: %s", info)
	}
	var parsed struct {
		Repos []struct {
			Name   string `json:"name"`
			Verify string `json:"verify_command"`
			Opened bool   `json:"opened"`
		} `json:"repos"`
		AllowedTools    []string          `json:"allowed_tools"`
		Gates           map[string]string `json:"gates"`
		ApprovalHistory []struct {
			ID, Decision, Comment string
		} `json:"approval_history"`
	}
	if err := json.Unmarshal([]byte(info), &parsed); err != nil {
		t.Fatalf("task_info is not JSON: %v\n%s", err, info)
	}
	if len(parsed.Repos) != 2 || parsed.Repos[1].Name != "web" || parsed.Repos[1].Opened || parsed.Gates["plan"] != "not approved" || len(parsed.AllowedTools) == 0 {
		t.Fatalf("task_info = %s", info)
	}
	if len(parsed.ApprovalHistory) != 1 || parsed.ApprovalHistory[0].Decision != "rejected" || parsed.ApprovalHistory[0].Comment != "thiếu test" {
		t.Fatalf("history = %+v", parsed.ApprovalHistory)
	}

	if text, isErr := call(t, cs, "complete_task", map[string]any{"summary": "xong", "pr_url": "https://x/pr/1"}); isErr {
		t.Fatalf("complete_task: %s", text)
	}
	c, err := st.Completion("mock-1")
	if err != nil || c == nil || c.PRURL != "https://x/pr/1" {
		t.Fatalf("completion = %+v, %v", c, err)
	}
}

func TestOpenRepoOverMCP(t *testing.T) {
	cs, st := setup(t)

	if text, isErr := call(t, cs, "open_repo", map[string]any{"name": "bom-api"}); !isErr || !strings.Contains(text, "not part of project") {
		t.Fatalf("repo outside the project: %q isErr=%v", text, isErr)
	}
	text, isErr := call(t, cs, "open_repo", map[string]any{"name": "web"})
	if isErr || !strings.Contains(text, "PASSES") {
		t.Fatalf("open web: %q", text)
	}
	text, isErr = call(t, cs, "open_repo", map[string]any{"name": "legacy"})
	if isErr || !strings.Contains(text, "ALREADY FAILS") || !strings.Contains(text, "bad format") {
		t.Fatalf("open legacy: %q", text)
	}
	b, _ := st.Baselines("mock-1")
	if b["web"] == nil || !b["web"].Passed || b["legacy"] == nil || b["legacy"].Passed {
		t.Fatalf("baselines = %+v", b)
	}
	// Opening again reuses the worktree and reports the recorded baseline.
	if text, _ := call(t, cs, "open_repo", map[string]any{"name": "legacy"}); !strings.Contains(text, "ALREADY FAILS") {
		t.Fatalf("reopen: %q", text)
	}
	info, _ := call(t, cs, "task_info", nil)
	if !strings.Contains(info, `"opened": true`) || !strings.Contains(info, `"baseline_verify_passed": false`) {
		t.Fatalf("task_info after open: %s", info)
	}
}

func TestCreatePullRequest(t *testing.T) {
	dir := t.TempDir()
	gitc := func(d string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Operator's clone with a Bitbucket-looking remote whose pushes land in a local bare repo.
	bare := filepath.Join(dir, "remote.git")
	exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).Run()
	clone := filepath.Join(dir, "svc")
	exec.Command("git", "init", "-q", "-b", "main", clone).Run()
	gitc(clone, "commit", "-q", "--allow-empty", "-m", "init")
	const url = "git@bitbucket.org:ws/svc.git"
	gitc(clone, "remote", "add", "origin", url)
	gitc(clone, "config", "url."+bare+".insteadOf", url)
	gitc(clone, "push", "-q", "origin", "main")
	gitc(clone, "fetch", "-q", "origin")

	// Fake Bitbucket that remembers open PRs per destination.
	var posted []map[string]any
	open := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			for dest, id := range open {
				if strings.Contains(r.URL.Query().Get("q"), `destination.branch.name = "`+dest+`"`) {
					fmt.Fprintf(w, `{"values":[{"id":%d,"title":"t","links":{"html":{"href":"https://bitbucket.org/ws/svc/pull-requests/%d"}}}]}`, id, id)
					return
				}
			}
			w.Write([]byte(`{"values":[]}`))
		case http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			posted = append(posted, body)
			id := len(posted)
			open[body["destination"].(map[string]any)["branch"].(map[string]any)["name"].(string)] = id
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"id":%d,"title":"feat: x","links":{"html":{"href":"https://bitbucket.org/ws/svc/pull-requests/%d"}}}`, id, id)
		}
	}))
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	ws := filepath.Join(st.Home, "worktrees", "mock-9")
	task := &store.Task{ID: "mock-9", TicketID: "HSK-1", Title: "cap discount", Project: "p", Branch: "tp/mock-9", Workspace: ws}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	flow := []config.PRStage{{Target: "release/qc", Gate: "pr"}, {Target: "main", Gate: "qc"}}
	cfg := &config.Config{Gates: []string{"plan", "pr", "qc"}, Projects: map[string]config.Project{"p": {Name: "P",
		Repos: map[string]config.Repo{"svc": {Path: clone, Base: "main", Remote: "origin", PRFlow: flow}}}}}
	h := &Handlers{Store: st, Config: cfg, TaskID: "mock-9", Bitbucket: func(context.Context) (*bitbucket.Client, error) {
		c := bitbucket.New("me", "tok")
		c.BaseURL = srv.URL
		return c, nil
	}}
	ctx := context.Background()
	in := CreatePullRequestIn{Repo: "svc", Title: "feat: x", Description: "why"}

	if _, err := h.CreatePullRequest(ctx, in); err == nil || !strings.Contains(err.Error(), `"pr" gate is not approved`) {
		t.Fatalf("before approval: %v", err)
	}
	for _, g := range []string{"plan", "pr"} {
		r, _ := st.AddRequest("mock-9", g, "s", "")
		st.Decide("mock-9", r, true, "")
	}
	if _, err := h.CreatePullRequest(ctx, in); err == nil || !strings.Contains(err.Error(), "not opened") {
		t.Fatalf("unopened repo: %v", err)
	}
	if _, err := h.OpenRepo(ctx, OpenRepoIn{Name: "svc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreatePullRequest(ctx, in); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("no commits: %v", err)
	}
	work := filepath.Join(ws, "svc")
	os.WriteFile(filepath.Join(work, "x.go"), []byte("package x\n"), 0o644)
	if _, err := h.CreatePullRequest(ctx, in); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty tree: %v", err)
	}
	gitc(work, "add", ".")
	gitc(work, "commit", "-q", "-m", "feat: x")

	// Stage 1: after the pr gate → PR into release/qc.
	msg, err := h.CreatePullRequest(ctx, in)
	if err != nil || !strings.Contains(msg, "→ release/qc") || !strings.Contains(msg, `after the "qc" gate`) {
		t.Fatalf("stage 1: %q %v", msg, err)
	}
	if got := gitc(bare, "rev-parse", "refs/heads/tp/mock-9"); got != gitc(work, "rev-parse", "HEAD") {
		t.Fatal("work branch was not pushed")
	}
	if !strings.Contains(posted[0]["description"].(string), "HSK-1") {
		t.Fatalf("payload = %v", posted[0])
	}
	// Asking for main before QC passed is refused.
	if _, err := h.CreatePullRequest(ctx, CreatePullRequestIn{Repo: "svc", Title: "x", Target: "main"}); err == nil || !strings.Contains(err.Error(), `"qc" gate`) {
		t.Fatalf("main before qc: %v", err)
	}
	// A QC fix: new commit, same call → pushes and reuses the open qc PR.
	os.WriteFile(filepath.Join(work, "y.go"), []byte("package x\n"), 0o644)
	gitc(work, "add", ".")
	gitc(work, "commit", "-q", "-m", "fix: qc feedback")
	if msg, err := h.CreatePullRequest(ctx, in); err != nil || !strings.Contains(msg, "existing pull request #1") {
		t.Fatalf("qc fix: %q %v", msg, err)
	}
	if len(posted) != 1 || gitc(bare, "rev-parse", "refs/heads/tp/mock-9") != gitc(work, "rev-parse", "HEAD") {
		t.Fatalf("qc fix must push without opening a second PR (posted %d)", len(posted))
	}
	// Stage 2: after the qc gate → PR from the feature branch into main.
	r, _ := st.AddRequest("mock-9", "qc", "QC ok?", "")
	st.Decide("mock-9", r, true, "QC passed")
	msg, err = h.CreatePullRequest(ctx, in)
	if err != nil || !strings.Contains(msg, "#2 tp/mock-9 → main") {
		t.Fatalf("stage 2: %q %v", msg, err)
	}
	if src := posted[1]["source"].(map[string]any)["branch"].(map[string]any)["name"]; src != "tp/mock-9" {
		t.Fatalf("main PR must come from the feature branch, got %v", src)
	}
	prs, _ := st.PullRequests("mock-9")
	if len(prs) != 2 || prs[0].Destination != "release/qc" || prs[1].Destination != "main" {
		t.Fatalf("pr records = %+v", prs)
	}
}

func TestCreatePullRequestWithoutTokenPushesAndRecordsCreatePage(t *testing.T) {
	dir := t.TempDir()
	gitc := func(d string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", d, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := filepath.Join(dir, "remote.git")
	exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).Run()
	clone := filepath.Join(dir, "svc")
	exec.Command("git", "init", "-q", "-b", "main", clone).Run()
	gitc(clone, "commit", "-q", "--allow-empty", "-m", "init")
	const url = "git@bitbucket.org:hasaki-tech/bom-api.git"
	gitc(clone, "remote", "add", "origin", url)
	gitc(clone, "config", "url."+bare+".insteadOf", url)
	gitc(clone, "push", "-q", "origin", "main")
	gitc(clone, "fetch", "-q", "origin")

	st, _ := store.New(t.TempDir())
	ws := filepath.Join(st.Home, "worktrees", "mock-4")
	task := &store.Task{ID: "mock-4", TicketID: "HSK-2", Project: "p", Branch: "tp/mock-4", Workspace: ws}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	cfg := &config.Config{Gates: []string{"pr"}, Projects: map[string]config.Project{"p": {Name: "P",
		Repos: map[string]config.Repo{"bom-api": {Path: clone, Base: "main", Remote: "origin", PRFlow: []config.PRStage{{Target: "main", Gate: "pr"}}}}}}}
	h := &Handlers{Store: st, Config: cfg, TaskID: "mock-4",
		Bitbucket: func(context.Context) (*bitbucket.Client, error) { return nil, bitbucket.ErrNoCredentials }}
	ctx := context.Background()
	r, _ := st.AddRequest("mock-4", "pr", "s", "")
	st.Decide("mock-4", r, true, "")
	h.OpenRepo(ctx, OpenRepoIn{Name: "bom-api"})
	work := filepath.Join(ws, "bom-api")
	os.WriteFile(filepath.Join(work, "x.go"), []byte("package x\n"), 0o644)
	gitc(work, "add", ".")
	gitc(work, "commit", "-q", "-m", "fix: x")

	msg, err := h.CreatePullRequest(ctx, CreatePullRequestIn{Repo: "bom-api", Title: "fix: x [HSK-2]", Description: "vì sao"})
	if err != nil || !strings.Contains(msg, "TUI (key p)") {
		t.Fatalf("fallback: %q %v", msg, err)
	}
	if gitc(bare, "rev-parse", "refs/heads/tp/mock-4") != gitc(work, "rev-parse", "HEAD") {
		t.Fatal("branch must still be pushed with the machine's git credentials")
	}
	pr, _ := st.PullRequestInto("mock-4", "bom-api", "main")
	want := "https://bitbucket.org/hasaki-tech/bom-api/pull-requests/new?dest=main&source=tp%2Fmock-4"
	if pr == nil || !pr.Pending || pr.URL != want || !strings.Contains(pr.Description, "HSK-2") {
		t.Fatalf("pending PR = %+v", pr)
	}
}

func TestSplunkSearchTool(t *testing.T) {
	var searched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			r.ParseForm()
			searched = append(searched, r.Form.Get("search"))
			w.Write([]byte(`{"sid":"9"}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Write([]byte(`{"results":[{"_time":"2026-11-09T10:00:00","_raw":"panic: nil map"}]}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"entry":[{"content":{"isDone":true}}]}`))
		}
	}))
	defer srv.Close()

	st, _ := store.New(t.TempDir())
	task := &store.Task{ID: "mock-s", Project: "payment"}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	cfg := &config.Config{
		Splunk:   config.Splunk{URL: srv.URL, MaxLookback: 48 * time.Hour, MaxEvents: 50, DefaultEarliest: "-1h", Timeout: time.Minute},
		Projects: map[string]config.Project{"payment": {Name: "Payment", SplunkFilter: `kubernetes.namespace_name="payment-*"`}},
	}
	h := &Handlers{Store: st, Config: cfg, TaskID: "mock-s", Splunk: func(context.Context) (*splunk.Client, error) {
		return &splunk.Client{BaseURL: srv.URL, Token: "tok", Poll: time.Millisecond}, nil
	}}
	ctx := context.Background()

	out, err := h.SplunkSearch(ctx, SplunkSearchIn{Query: "level=error OR level=fatal | head 5"})
	if err != nil || !strings.Contains(out, "panic: nil map") {
		t.Fatalf("search: %q %v", out, err)
	}
	if want := `search (kubernetes.namespace_name="payment-*") (level=error OR level=fatal) | head 5`; searched[0] != want {
		t.Fatalf("sent %q\nwant %q", searched[0], want)
	}
	for _, bad := range []SplunkSearchIn{
		{Query: "x | outputlookup a.csv"},
		{Query: "x", Earliest: "-30d"},
		{Query: "x", Earliest: "yesterday"},
		{Query: "x", Earliest: "2020-01-01T00:00"},
	} {
		if _, err := h.SplunkSearch(ctx, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if len(searched) != 1 {
		t.Fatalf("rejected searches must never reach Splunk, sent %d", len(searched))
	}
	plm := &store.Task{ID: "mock-plm", Project: "plm"}
	plm.Transition(store.StateRunning, "")
	st.SaveTask(plm)
	cfg.Projects["plm"] = config.Project{Name: "PLM", NoLogs: true}
	hp := &Handlers{Store: st, Config: cfg, TaskID: "mock-plm", Splunk: h.Splunk}
	if _, err := hp.SplunkSearch(ctx, SplunkSearchIn{Query: "level=error"}); err == nil || !strings.Contains(err.Error(), "no logs") || len(searched) != 1 {
		t.Fatalf("PLM search must be refused locally: %v (sent %d)", err, len(searched))
	}
	qs, _ := st.SplunkQueries("mock-s")
	if len(qs) != 1 || !strings.Contains(qs[0].Query, `payment-*`) || qs[0].Events != 1 {
		t.Fatalf("audit log = %+v", qs)
	}
}

func TestQCToolsAreScopedToTheProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/applications":
			w.Write([]byte(`{"items":[{"metadata":{"name":"bom-api-qc"},"status":{"health":{"status":"Healthy"},"sync":{"status":"Synced","revision":"abcdef1234567890"},"resources":[{"group":"apps","kind":"Deployment","name":"bom-api-qc","namespace":"bom-qc"}]}},{"metadata":{"name":"payment-qc"},"status":{}}]}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Write([]byte(`{"result":{"content":"boom MO-1","timeStamp":"t","podName":"p"}}` + "\n"))
		}
	}))
	defer srv.Close()
	st, _ := store.New(t.TempDir())
	task := &store.Task{ID: "mock-q", Project: "bom"}
	task.Transition(store.StateRunning, "")
	st.SaveTask(task)
	cfg := &config.Config{ArgoCD: config.ArgoCD{URL: srv.URL, MaxLogLines: 50}, Projects: map[string]config.Project{
		"bom":     {Name: "BOM", Repos: map[string]config.Repo{"bom-api": {}}},
		"payment": {Name: "Payment", Repos: map[string]config.Repo{"payment": {}}},
	}}
	h := &Handlers{Store: st, Config: cfg, TaskID: "mock-q", ArgoCD: func(context.Context) (*argocd.Client, error) {
		return &argocd.Client{BaseURL: srv.URL, Token: "t"}, nil
	}}
	ctx := context.Background()
	out, err := h.QCApps(ctx)
	if err != nil || !strings.Contains(out, "bom-api-qc: health=Healthy sync=Synced revision=abcdef1234") || strings.Contains(out, "payment-qc") {
		t.Fatalf("qc_apps: %q %v", out, err)
	}
	if _, err := h.QCLogs(ctx, QCLogsIn{App: "payment-qc"}); err == nil || !strings.Contains(err.Error(), "not one of this project") {
		t.Fatalf("another project's app must be refused: %v", err)
	}
	out, err = h.QCLogs(ctx, QCLogsIn{App: "bom-api-qc", Filter: "MO-1", Tail: 10000})
	if err != nil || !strings.Contains(out, "boom MO-1") {
		t.Fatalf("qc_logs: %q %v", out, err)
	}
}
