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
	p, err := OpenRepo(context.Background(), ws, "mock-1", "svc", config.Repo{Path: clone, Base: "main", Remote: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Created || p.Path != filepath.Join(ws, "svc") {
		t.Fatalf("opened = %+v", p)
	}
	again, err := OpenRepo(context.Background(), ws, "mock-1", "svc", config.Repo{Path: clone, Base: "main", Remote: "origin"})
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
	if _, err := OpenRepo(context.Background(), t.TempDir(), "x", "../evil", config.Repo{}); err == nil {
		t.Fatal("path traversal in repo name accepted")
	}
}
