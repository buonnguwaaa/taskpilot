package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreeStartsFromFetchedRemoteBase(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	run(t, dir, "init", "-q", "--bare", "-b", "main", remote)

	// The operator's clone, currently on another branch with a stale local main.
	clone := filepath.Join(dir, "clone")
	run(t, dir, "clone", "-q", remote, clone)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("1"), 0o644)
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-q", "-m", "one")
	run(t, clone, "push", "-q", "origin", "main")
	run(t, clone, "checkout", "-q", "-b", "release/qc")

	// Someone else pushes to main afterwards.
	other := filepath.Join(dir, "other")
	run(t, dir, "clone", "-q", remote, other)
	os.WriteFile(filepath.Join(other, "b.txt"), []byte("2"), 0o644)
	run(t, other, "add", ".")
	run(t, other, "commit", "-q", "-m", "two")
	run(t, other, "push", "-q", "origin", "main")
	want := run(t, other, "rev-parse", "HEAD")

	ws, err := Create(filepath.Join(dir, "home"), "mock-1")
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenRepo(context.Background(), ws, Branch("mock-1"), "svc", config.Repo{Path: clone, Base: "main", Remote: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Created || p.Path != filepath.Join(ws, "svc") {
		t.Fatalf("opened = %+v", p)
	}
	again, err := OpenRepo(context.Background(), ws, Branch("mock-1"), "svc", config.Repo{Path: clone, Base: "main", Remote: "origin"})
	if err != nil || again.Created {
		t.Fatalf("second open should reuse the worktree: %+v %v", again, err)
	}
	if got := OpenedRepos(ws); len(got) != 1 || got[0] != "svc" {
		t.Fatalf("opened repos = %v", got)
	}
	if got := run(t, p.Path, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree starts at %s, want fetched origin/main %s (notes %v)", got, want, p.Notes)
	}
	if b := run(t, p.Path, "branch", "--show-current"); b != "tp/mock-1" {
		t.Fatalf("branch = %s", b)
	}
	if up, err := exec.Command("git", "-C", p.Path, "rev-parse", "--abbrev-ref", "@{upstream}").CombinedOutput(); err == nil {
		t.Fatalf("work branch must not track the base, tracks %s", up)
	}
	if cur := run(t, clone, "branch", "--show-current"); cur != "release/qc" {
		t.Fatalf("operator's checkout changed to %s", cur)
	}
}

func TestRunVerify(t *testing.T) {
	dir := t.TempDir()
	if r := RunVerify(context.Background(), dir, "echo ok && true", time.Minute); !r.Passed || !strings.Contains(r.Output, "ok") {
		t.Fatalf("%+v", r)
	}
	if r := RunVerify(context.Background(), dir, "echo boom; exit 3", time.Minute); r.Passed || !strings.Contains(r.Output, "boom") {
		t.Fatalf("%+v", r)
	}
	if r := RunVerify(context.Background(), dir, "sleep 5", 100*time.Millisecond); r.Passed || !strings.Contains(r.Output, "timed out") {
		t.Fatalf("%+v", r)
	}
}

func TestOpenRepoRejectsBadName(t *testing.T) {
	if _, err := OpenRepo(context.Background(), t.TempDir(), Branch("x"), "../evil", config.Repo{}); err == nil {
		t.Fatal("path traversal in repo name accepted")
	}
}

func TestRemoveWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	run(t, dir, "init", "-q", "-b", "main", repo)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1"), 0o644)
	run(t, repo, "add", ".")
	run(t, repo, "commit", "-q", "-m", "one")

	ws, err := Create(filepath.Join(dir, "home"), "mock-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRepo(context.Background(), ws, Branch("mock-1"), "svc", config.Repo{Path: repo, Base: "main", Remote: "-"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "svc", "dirty.txt"), []byte("x"), 0o644) // uncommitted work

	if err := Remove(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("workspace directory must be gone")
	}
	if out := run(t, repo, "worktree", "list"); strings.Contains(out, "svc") {
		t.Fatalf("worktree still registered:\n%s", out)
	}
	if out := run(t, repo, "branch", "--list", Branch("mock-1")); out == "" {
		t.Fatal("the work branch must be kept")
	}
	if err := Remove(context.Background(), ""); err != nil {
		t.Fatal("no workspace: nothing to do")
	}
}

func TestReleaseFreesCleanWorktrees(t *testing.T) {
	dir := t.TempDir()
	var repos []string
	for _, n := range []string{"clean", "dirty"} {
		repo := filepath.Join(dir, n)
		run(t, dir, "init", "-q", "-b", "main", repo)
		os.WriteFile(filepath.Join(repo, "a.txt"), []byte("1"), 0o644)
		run(t, repo, "add", ".")
		run(t, repo, "commit", "-q", "-m", "one")
		repos = append(repos, repo)
	}
	ws, _ := Create(filepath.Join(dir, "home"), "mock-1")
	for i, n := range []string{"clean", "dirty"} {
		if _, err := OpenRepo(context.Background(), ws, Branch("mock-1"), n, config.Repo{Path: repos[i], Base: "main", Remote: "-"}); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(ws, "dirty", "wip.txt"), []byte("x"), 0o644)

	kept, err := Release(context.Background(), ws)
	if err != nil || len(kept) != 1 || kept[0] != "dirty" {
		t.Fatalf("kept=%v err=%v", kept, err)
	}
	// The branch is free again: the operator's clone can check it out.
	run(t, repos[0], "checkout", "-q", Branch("mock-1"))
	if _, err := os.Stat(filepath.Join(ws, "dirty", "wip.txt")); err != nil {
		t.Fatal("uncommitted work must stay")
	}
}

func TestReviewCheckoutWhileTheWorktreeHoldsTheBranch(t *testing.T) {
	dir := t.TempDir()
	clone := filepath.Join(dir, "clone")
	run(t, dir, "init", "-q", "-b", "main", clone)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("1"), 0o644)
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-q", "-m", "one")
	run(t, clone, "checkout", "-q", "-b", "feature/mine") // the operator's own branch

	ws, _ := Create(filepath.Join(dir, "home"), "mock-1")
	o, err := OpenRepo(context.Background(), ws, Branch("mock-1"), "svc", config.Repo{Path: clone, Base: "main", Remote: "-"})
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) string {
		os.WriteFile(filepath.Join(o.Path, "b.txt"), []byte(msg), 0o644)
		run(t, o.Path, "add", ".")
		run(t, o.Path, "commit", "-q", "-m", msg)
		return run(t, o.Path, "rev-parse", "HEAD")
	}
	first := commit("agent 1")
	br := Branch("mock-1")
	if out, err := exec.Command("git", "-C", clone, "checkout", br).CombinedOutput(); err == nil {
		t.Fatalf("plain checkout should be refused while the worktree holds it: %s", out)
	}

	ctx := context.Background()
	if act, err := ReviewCheckout(ctx, clone, br); err != nil || act != ReviewOpened || run(t, clone, "rev-parse", "HEAD") != first {
		t.Fatalf("open: %v %v", act, err)
	}
	second := commit("agent 2") // the agent keeps working in its worktree
	if act, err := ReviewCheckout(ctx, clone, br); err != nil || act != ReviewUpdated || run(t, clone, "rev-parse", "HEAD") != second {
		t.Fatalf("update: %v %v", act, err)
	}
	if act, err := ReviewCheckout(ctx, clone, br); err != nil || act != ReviewReturned || run(t, clone, "branch", "--show-current") != "feature/mine" {
		t.Fatalf("return: %v %v", act, err)
	}
	// A dirty clone is left alone.
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("wip"), 0o644)
	if _, err := ReviewCheckout(ctx, clone, br); err == nil || !strings.Contains(err.Error(), "chưa commit") {
		t.Fatalf("dirty clone: %v", err)
	}
	if run(t, clone, "branch", "--show-current") != "feature/mine" {
		t.Fatal("a dirty clone must not move")
	}
}

func TestRenameBranchAllOrNothing(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	var repos []string
	for _, n := range []string{"a", "b"} {
		r := filepath.Join(t.TempDir(), n)
		exec.Command("git", "init", "-q", "-b", "main", r).Run()
		exec.Command("git", "-C", r, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init").Run()
		if _, err := OpenRepo(ctx, ws, Branch("m1"), n, config.Repo{Path: r, Base: "main", Remote: "-"}); err != nil {
			t.Fatal(err)
		}
		repos = append(repos, r)
	}
	// The new name taken in one repo: nothing is renamed.
	exec.Command("git", "-C", repos[1], "branch", "feat/x--ABC_DEF_0001", "main").Run()
	if _, err := RenameBranch(ctx, ws, Branch("m1"), "feat/x--ABC_DEF_0001"); err == nil {
		t.Fatal("a taken name must be refused")
	}
	head := func(n string) string {
		out, _ := exec.Command("git", "-C", filepath.Join(ws, n), "symbolic-ref", "--short", "HEAD").Output()
		return strings.TrimSpace(string(out))
	}
	if head("a") != Branch("m1") {
		t.Fatalf("a renamed on refusal: %s", head("a"))
	}
	got, err := RenameBranch(ctx, ws, Branch("m1"), "feat/y--ABC_DEF_0001")
	if err != nil || len(got) != 2 || head("a") != "feat/y--ABC_DEF_0001" || head("b") != "feat/y--ABC_DEF_0001" {
		t.Fatalf("rename: %v %v %s %s", got, err, head("a"), head("b"))
	}
	if _, err := RenameBranch(ctx, ws, "feat/y--ABC_DEF_0001", "bad..name"); err == nil {
		t.Fatal("invalid ref name must be refused")
	}
}

// A task never starts from a stale base: when the remote cannot be fetched
// the repo is not opened (it used to fall back to the last fetched base).
func TestOpenRepoRefusesAStaleBase(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	run(t, dir, "init", "-q", "--bare", "-b", "main", remote)
	clone := filepath.Join(dir, "clone")
	run(t, dir, "clone", "-q", remote, clone)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("1"), 0o644)
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-q", "-m", "one")
	run(t, clone, "push", "-q", "origin", "main")
	os.RemoveAll(remote) // the remote is gone: origin/main is stale
	old := FetchAttempts
	FetchAttempts = 1
	defer func() { FetchAttempts = old }()
	ws, _ := Create(filepath.Join(dir, "home"), "mock-1")
	_, err := OpenRepo(context.Background(), ws, Branch("mock-1"), "svc", config.Repo{Path: clone, Base: "main", Remote: "origin"})
	if err == nil || !strings.Contains(err.Error(), "cannot fetch the latest origin/main") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "svc")); err == nil {
		t.Fatal("no worktree from a stale base")
	}
}
