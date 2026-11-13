package graphify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// fakeGraphify logs its calls and writes minimal graphs.
const fakeGraphify = `#!/bin/sh
echo "$*" >> "$GRAPHIFY_LOG"
out=""
case "$1" in
extract)
  src=$2
  while [ $# -gt 0 ]; do [ "$1" = --out ] && out=$2; shift; done
  mkdir -p "$out/graphify-out"
  printf '{"nodes":[{"id":"n","source_file":"a.go","source_location":"L1"}],"links":[]}' > "$out/graphify-out/graph.json"
  ;;
merge-graphs)
  while [ $# -gt 0 ]; do [ "$1" = --out ] && out=$2; shift; done
  printf '{"nodes":[],"links":[]}' > "$out"
  ;;
esac
`

func TestTaskGraphFollowsTheWorktree(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "graphify")
	os.WriteFile(bin, []byte(fakeGraphify), 0o755)
	logf := filepath.Join(root, "calls.log")
	t.Setenv("GRAPHIFY_LOG", logf)
	calls := func() string { b, _ := os.ReadFile(logf); return string(b) }

	// The operator's repo of api; worker is not opened by the task.
	api := filepath.Join(root, "api")
	os.MkdirAll(api, 0o755)
	run(t, api, "git", "init", "-q", "-b", "main")
	write(t, api, map[string]string{"a.go": "package a\n", ".gitignore": "secret.txt\n"})
	run(t, api, "git", "add", ".")
	run(t, api, "git", "commit", "-qm", "a")

	home := filepath.Join(root, "home")
	g := &config.Graphify{Dir: filepath.Join(root, "graphs"), Bin: bin, Ignore: []string{"*.pb.go"}, Lines: map[string]string{"prod": "release/prod"}}
	cfg := &config.Config{Home: home, Graphify: g, Projects: map[string]config.Project{"bom": {Repos: map[string]config.Repo{
		"api": {Path: api, Base: "main", Remote: "-"}, "worker": {Path: filepath.Join(root, "worker"), Base: "main"}}}}}
	// The prod line has both repos: worker stands in as is, api's cache seeds the task's.
	write(t, filepath.Join(outDir(g.Dir, "bom", "prod", "worker"), "graphify-out"), map[string]string{"graph.json": `{"nodes":[],"links":[]}`})
	write(t, SrcDir(g.Dir, "bom", "prod", "worker"), map[string]string{"w.go": "package w\n"})
	write(t, filepath.Join(outDir(g.Dir, "bom", "prod", "api"), "graphify-out", "cache", "ast"), map[string]string{"x.json": "{}"})

	ctx := context.Background()
	ws, _ := workspace.Create(home, "mock-t")
	if ok, err := BuildTask(ctx, cfg, "bom", "mock-t", ws, nil); ok || err != nil {
		t.Fatalf("no repo opened: ok=%v err=%v", ok, err)
	}
	if _, err := workspace.OpenRepo(ctx, ws, workspace.Branch("mock-t"), "api", cfg.Projects["bom"].Repos["api"]); err != nil {
		t.Fatal(err)
	}
	if ok, err := BuildTask(ctx, cfg, "bom", "mock-t", ws, nil); !ok || err != nil {
		t.Fatalf("build: ok=%v err=%v", ok, err)
	}
	c := calls()
	if strings.Count(c, "extract") != 1 || !strings.Contains(c, "extract "+taskSrc(g.Dir, "mock-t", "api")) ||
		!strings.Contains(c, "merge-graphs") || !strings.Contains(c, repoGraph(g.Dir, "bom", "prod", "worker")) {
		t.Fatalf("calls:\n%s", c)
	}
	if _, err := os.Stat(filepath.Join(taskOut(g.Dir, "mock-t", "api"), "graphify-out", "cache", "ast", "x.json")); err != nil {
		t.Fatal("the prod line's cache seeds the task's")
	}
	if _, err := os.Stat(filepath.Join(taskSrc(g.Dir, "mock-t", "api"), ".graphifyignore")); err != nil {
		t.Fatal("the ignore file goes in the copy")
	}
	if _, err := os.Stat(filepath.Join(ws, "api", ".graphifyignore")); err == nil {
		t.Fatal("nothing is written into the agent's worktree")
	}
	if _, err := os.Stat(TaskGraphPath(g.Dir, "mock-t")); err != nil {
		t.Fatal("merged task graph")
	}

	// Nothing changed: no work.
	BuildTask(ctx, cfg, "bom", "mock-t", ws, nil)
	if strings.Count(calls(), "extract") != 1 {
		t.Fatal("an unchanged worktree is not parsed again")
	}
	// The agent edits (uncommitted), adds a file, and an ignored file appears.
	write(t, filepath.Join(ws, "api"), map[string]string{"a.go": "package a\n\nfunc New() {}\n", "b.go": "package a\n", "secret.txt": "x"})
	BuildTask(ctx, cfg, "bom", "mock-t", ws, nil)
	if strings.Count(calls(), "extract") != 2 {
		t.Fatalf("an edited worktree is parsed again:\n%s", calls())
	}
	src := taskSrc(g.Dir, "mock-t", "api")
	if b, _ := os.ReadFile(filepath.Join(src, "a.go")); !strings.Contains(string(b), "func New") {
		t.Fatal("the copy has the uncommitted change")
	}
	if _, err := os.Stat(filepath.Join(src, "b.go")); err != nil {
		t.Fatal("untracked files are copied")
	}
	if _, err := os.Stat(filepath.Join(src, "secret.txt")); err == nil {
		t.Fatal("ignored files are not")
	}

	// The workspace is removed: the task graph is cleaned up.
	os.RemoveAll(ws)
	CleanTasks(g.Dir, home)
	if _, err := os.Stat(TaskDir(g.Dir, "mock-t")); err == nil {
		t.Fatal("task graph left behind")
	}
}
