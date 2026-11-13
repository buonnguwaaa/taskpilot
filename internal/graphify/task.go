package graphify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// Task graphs: the code of a task's own branch. A task starts from the
// latest main and changes it; its graph is the repos it opened, taken from
// its worktrees (committed and uncommitted changes), merged with the other
// repos of the project from the base line, and linked like a line.
//
//	<dir>/tasks/<task>/src/<repo>/        copy of the worktree (tracked + untracked, not ignored)
//	<dir>/tasks/<task>/out/<repo>/        graph + graphify cache (seeded from the base line's)
//	<dir>/tasks/<task>/graph.json         merged graph
//	<dir>/tasks/<task>/links.json, state.json
//
// It is rebuilt only when a worktree changed (HEAD, diff, untracked files),
// and only the changed files are parsed again (graphify's cache).

// TaskLine is the name agents use for the task's own graph.
const TaskLine = "task"

// BaseLine is the line whose graphs stand in for the repos a task did not
// open, and seed the cache of the ones it did (tasks start from main, which
// release/prod follows).
const BaseLine = "prod"

// TaskState is what a task graph was built from.
type TaskState struct {
	Repos    map[string]string `json:"repos"` // repo → worktree fingerprint
	MergedAt time.Time         `json:"merged_at"`
	Nodes    int               `json:"nodes"`
	Links    int               `json:"links"`
}

func TaskDir(dir, taskID string) string { return filepath.Join(dir, "tasks", taskID) }
func TaskGraphPath(dir, taskID string) string {
	return filepath.Join(TaskDir(dir, taskID), "graph.json")
}
func TaskLinksPath(dir, taskID string) string {
	return filepath.Join(TaskDir(dir, taskID), "links.json")
}
func taskSrc(dir, taskID, repo string) string {
	return filepath.Join(TaskDir(dir, taskID), "src", repo)
}
func taskOut(dir, taskID, repo string) string {
	return filepath.Join(TaskDir(dir, taskID), "out", repo)
}

// BuildTask brings the task graph up to date with the task's worktrees. It
// returns false when the task opened no repo yet (use a line instead).
func BuildTask(ctx context.Context, cfg *config.Config, project, taskID, ws string, logf func(string, ...any)) (bool, error) {
	g := cfg.Graphify
	opened := workspace.OpenedRepos(ws)
	if g == nil || len(opened) == 0 {
		return false, nil
	}
	td := TaskDir(g.Dir, taskID)
	if err := os.MkdirAll(td, 0o755); err != nil {
		return false, err
	}
	lf, err := os.OpenFile(filepath.Join(td, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	st := &TaskState{Repos: map[string]string{}}
	if b, err := os.ReadFile(filepath.Join(td, "state.json")); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Repos == nil {
		st.Repos = map[string]string{}
	}
	changed := false
	inTask := map[string]bool{}
	for _, repo := range opened {
		inTask[repo] = true
		wt := filepath.Join(ws, repo)
		fp, err := fingerprint(ctx, wt)
		if err != nil {
			return false, fmt.Errorf("%s: %w", repo, err)
		}
		graph := filepath.Join(taskOut(g.Dir, taskID, repo), "graphify-out", "graph.json")
		if _, err := os.Stat(graph); err == nil && st.Repos[repo] == fp {
			continue
		}
		if err := buildWorktree(ctx, g, project, taskID, repo, wt); err != nil {
			return false, fmt.Errorf("%s: %w", repo, err)
		}
		st.Repos[repo] = fp
		changed = true
	}
	for repo := range st.Repos {
		if !inTask[repo] {
			delete(st.Repos, repo)
			changed = true
		}
	}
	if _, err := os.Stat(TaskGraphPath(g.Dir, taskID)); err != nil {
		changed = true
	}
	if !changed {
		return true, nil
	}
	var parts []part
	for _, repo := range cfg.Projects[project].RepoNames() {
		if inTask[repo] {
			parts = append(parts, part{name: repo, graph: filepath.Join(taskOut(g.Dir, taskID, repo), "graphify-out", "graph.json"),
				src: taskSrc(g.Dir, taskID, repo)})
		} else {
			parts = append(parts, part{name: repo, graph: repoGraph(g.Dir, project, BaseLine, repo), src: SrcDir(g.Dir, project, BaseLine, repo)})
		}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	nodes, _, added, err := mergeParts(ctx, g.Bin, td, parts, TaskGraphPath(g.Dir, taskID), TaskLinksPath(g.Dir, taskID), logf)
	if err != nil {
		return false, err
	}
	st.MergedAt, st.Nodes, st.Links = time.Now(), nodes, added
	return true, writeJSON(filepath.Join(td, "state.json"), st)
}

// buildWorktree copies a worktree's files and parses them, with the base
// line's cache so unchanged files are not parsed again.
func buildWorktree(ctx context.Context, g *config.Graphify, project, taskID, repo, wt string) error {
	src := taskSrc(g.Dir, taskID, repo)
	tmp := src + ".new"
	_ = os.RemoveAll(tmp)
	if err := copyWorktree(ctx, wt, tmp); err != nil {
		return err
	}
	if len(g.Ignore) > 0 {
		_ = os.WriteFile(filepath.Join(tmp, ".graphifyignore"), []byte(strings.Join(g.Ignore, "\n")+"\n"), 0o644)
	}
	_ = os.RemoveAll(src)
	if err := os.Rename(tmp, src); err != nil {
		return err
	}
	out := taskOut(g.Dir, taskID, repo)
	cache := filepath.Join(out, "graphify-out", "cache", "ast")
	if _, err := os.Stat(cache); err != nil {
		base := filepath.Join(outDir(g.Dir, project, BaseLine, repo), "graphify-out", "cache", "ast")
		if _, err := os.Stat(base); err == nil {
			_ = copyDir(base, cache)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, g.Bin, "extract", src, "--code-only", "--no-cluster", "--out", out)
	cmd.Env = append(os.Environ(), "GRAPHIFY_FORCE=1")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("graphify extract: %w: %s", err, tailText(buf.String(), 500))
	}
	return nil
}

// fingerprint changes when a worktree's code does: HEAD, the diff against
// it, and the untracked files (names, sizes, times).
func fingerprint(ctx context.Context, wt string) (string, error) {
	h := sha256.New()
	head, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	io.WriteString(h, head)
	diff, err := git(ctx, wt, "diff", "HEAD", "--binary")
	if err != nil {
		return "", err
	}
	io.WriteString(h, diff)
	others, _ := git(ctx, wt, "ls-files", "-o", "--exclude-standard")
	for _, f := range strings.Split(strings.TrimSpace(others), "\n") {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(wt, f)); err == nil {
			fmt.Fprintf(h, "%s %d %d\n", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyWorktree copies the tracked and untracked (not ignored) files.
func copyWorktree(ctx context.Context, wt, dst string) error {
	out, err := git(ctx, wt, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		from := filepath.Join(wt, f)
		fi, err := os.Lstat(from)
		if err != nil || !fi.Mode().IsRegular() {
			continue // deleted in the worktree, or a symlink
		}
		if err := copyFileMkdir(from, filepath.Join(dst, f)); err != nil {
			return err
		}
	}
	return nil
}

func copyFileMkdir(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	o, err := os.Create(to)
	if err != nil {
		return err
	}
	_, err = io.Copy(o, in)
	if cerr := o.Close(); err == nil {
		err = cerr
	}
	return err
}

func copyDir(from, to string) error {
	return filepath.Walk(from, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		return copyFileMkdir(p, filepath.Join(to, rel))
	})
}

// CleanTasks removes the graphs of tasks whose workspace is gone.
func CleanTasks(dir, home string) {
	entries, err := os.ReadDir(filepath.Join(dir, "tasks"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if _, err := os.Stat(workspace.Dir(home, e.Name())); os.IsNotExist(err) {
			_ = os.RemoveAll(filepath.Join(dir, "tasks", e.Name()))
		}
	}
}
