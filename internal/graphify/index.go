// Package graphify builds and serves the graphify graphs of the projects.
//
// Each repo is indexed per line: a long-lived branch such as release/prod or
// release/qc, taken from the remote (never the operator's checkout, which
// may be on any branch). graphify parses the snapshot locally (code only, no
// LLM) into a graph per repo; the graphs of a project's line are merged and
// the gRPC links between its repos (grpc.go) are added as calls_grpc edges,
// so graphify's path / affected / explain cross repos.
//
//	<dir>/<project>/<line>/src/<repo>/                    snapshot of the branch
//	<dir>/<project>/<line>/out/<repo>/graphify-out/       graph + graphify cache
//	<dir>/<project>/<line>/graph.json                     merged graph of the line
//	<dir>/<project>/<line>/links.json                     gRPC links between repos
//	<dir>/<project>/<line>/state.json                     what was built from which commit
package graphify

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// RepoState is what a repo's graph of a line was built from.
type RepoState struct {
	Branch  string    `json:"branch"`
	Commit  string    `json:"commit"`
	BuiltAt time.Time `json:"built_at"`
	Nodes   int       `json:"nodes"`
	Edges   int       `json:"edges"`
	Error   string    `json:"error,omitempty"`
}

// LineState is the state of a project's line.
type LineState struct {
	Repos    map[string]*RepoState `json:"repos"`
	MergedAt time.Time             `json:"merged_at"`
	Links    int                   `json:"links"`
	Nodes    int                   `json:"nodes"`
	Edges    int                   `json:"edges"`
	Error    string                `json:"error,omitempty"`
	Checked  time.Time             `json:"checked"`
}

// Paths of a project's line.
func LineDir(dir, project, line string) string { return filepath.Join(dir, project, line) }
func SrcDir(dir, project, line, repo string) string {
	return filepath.Join(LineDir(dir, project, line), "src", repo)
}
func outDir(dir, project, line, repo string) string {
	return filepath.Join(LineDir(dir, project, line), "out", repo)
}
func GraphPath(dir, project, line string) string {
	return filepath.Join(LineDir(dir, project, line), "graph.json")
}
func LinksPath(dir, project, line string) string {
	return filepath.Join(LineDir(dir, project, line), "links.json")
}
func statePath(dir, project, line string) string {
	return filepath.Join(LineDir(dir, project, line), "state.json")
}

// ReadState returns the state of a line (empty when never built).
func ReadState(dir, project, line string) *LineState {
	st := &LineState{Repos: map[string]*RepoState{}}
	if b, err := os.ReadFile(statePath(dir, project, line)); err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Repos == nil {
		st.Repos = map[string]*RepoState{}
	}
	return st
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadLinks returns the gRPC links of a line.
func ReadLinks(dir, project, line string) ([]Link, error) {
	return ReadLinksFile(LinksPath(dir, project, line))
}

// ReadLinksFile reads a links.json.
func ReadLinksFile(path string) ([]Link, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ls []Link
	return ls, json.Unmarshal(b, &ls)
}

// Indexer keeps the graphs up to date.
type Indexer struct {
	Config func() *config.Config
	Logf   func(format string, args ...any)
	Now    func() time.Time
}

func (x *Indexer) logf(f string, a ...any) {
	if x.Logf != nil {
		x.Logf(f, a...)
	}
}

func (x *Indexer) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// Refresh brings every line of a project up to date now (a new task starts
// from the latest branches: the graphs must be of them too). It returns one
// note per line.
func (x *Indexer) Refresh(ctx context.Context, project string) []string {
	cfg := x.Config()
	if cfg.Graphify == nil {
		return nil
	}
	var notes []string
	for _, line := range cfg.ProjectGraphLines(project) {
		start := x.now()
		st, err := x.Build(ctx, project, line, false)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("graphify %s: %v", line, err))
		default:
			var moved []string
			for name, rs := range st.Repos {
				if rs.Error == "" && !rs.BuiltAt.Before(start) {
					moved = append(moved, name+"@"+short(rs.Commit))
				}
			}
			sort.Strings(moved)
			if len(moved) == 0 {
				notes = append(notes, fmt.Sprintf("graphify %s: up to date", line))
			} else {
				notes = append(notes, fmt.Sprintf("graphify %s: rebuilt %s", line, strings.Join(moved, ", ")))
			}
		}
	}
	return notes
}

// Tick refreshes the lines whose last check is older than graphify.refresh.
func (x *Indexer) Tick(ctx context.Context) {
	cfg := x.Config()
	if cfg.Graphify == nil {
		return
	}
	CleanTasks(cfg.Graphify.Dir, cfg.Home)
	for _, p := range cfg.ProjectNames() {
		for _, line := range cfg.ProjectGraphLines(p) {
			st := ReadState(cfg.Graphify.Dir, p, line)
			if x.now().Sub(st.Checked) < cfg.Graphify.Refresh {
				continue
			}
			if _, err := x.Build(ctx, p, line, false); err != nil {
				x.logf("graphify %s/%s: %v", p, line, err)
			}
		}
	}
}

// Build updates a project's line: the repos whose branch moved are exported
// and parsed again (force: all of them), then the line is merged and linked.
func (x *Indexer) Build(ctx context.Context, project, line string, force bool) (*LineState, error) {
	cfg := x.Config()
	g := cfg.Graphify
	if g == nil {
		return nil, errors.New("graphify is not configured")
	}
	proj, ok := cfg.Projects[project]
	if !ok {
		return nil, fmt.Errorf("unknown project %q", project)
	}
	if err := os.MkdirAll(LineDir(g.Dir, project, line), 0o755); err != nil {
		return nil, err
	}
	// The daemon and `taskpilot graphify build` may run at the same time.
	lf, err := os.OpenFile(filepath.Join(LineDir(g.Dir, project, line), ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	// Wait for a build in progress (the daemon's, a task start's or the
	// CLI's): it leaves the line up to date, so this one then has little to do.
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	st := ReadState(g.Dir, project, line)
	changed := false
	for _, name := range proj.RepoNames() {
		branch, ok := cfg.GraphLines(project, name)[line]
		if !ok {
			if st.Repos[name] != nil {
				delete(st.Repos, name)
				changed = true
			}
			continue
		}
		repo := proj.Repos[name]
		commit, used, err := firstBranch(ctx, repo, branch)
		branch = used
		if err != nil {
			// A repo without that branch is simply not on this line.
			if st.Repos[name] == nil || st.Repos[name].Error != err.Error() {
				st.Repos[name] = &RepoState{Branch: branch, Error: err.Error()}
				changed = true
			}
			continue
		}
		if rs := st.Repos[name]; !force && rs != nil && rs.Commit == commit && rs.Error == "" {
			continue
		}
		rs := &RepoState{Branch: branch, Commit: commit, BuiltAt: x.now()}
		if err := x.buildRepo(ctx, g, project, line, name, repo, commit, rs); err != nil {
			rs.Error = err.Error()
			x.logf("graphify %s/%s/%s: %v", project, line, name, err)
		}
		st.Repos[name] = rs
		changed = true
	}
	st.Checked = x.now()
	if changed || force || st.MergedAt.IsZero() {
		if err := x.merge(ctx, g, project, line, st); err != nil {
			st.Error = err.Error()
		} else {
			st.Error = ""
		}
	}
	if err := writeJSON(statePath(g.Dir, project, line), st); err != nil {
		return st, err
	}
	if st.Error != "" {
		return st, errors.New(st.Error)
	}
	return st, nil
}

func (x *Indexer) buildRepo(ctx context.Context, g *config.Graphify, project, line, name string, repo config.Repo, commit string, rs *RepoState) error {
	src := SrcDir(g.Dir, project, line, name)
	tmp := src + ".new"
	_ = os.RemoveAll(tmp)
	if err := exportTree(ctx, repo.Path, commit, tmp); err != nil {
		return fmt.Errorf("export %s: %w", commit, err)
	}
	if len(g.Ignore) > 0 {
		_ = os.WriteFile(filepath.Join(tmp, ".graphifyignore"), []byte(strings.Join(g.Ignore, "\n")+"\n"), 0o644)
	}
	_ = os.RemoveAll(src)
	if err := os.Rename(tmp, src); err != nil {
		return err
	}
	out := outDir(g.Dir, project, line, name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	// The graphify cache (by file content) stays in out/ between builds.
	cmd := exec.CommandContext(cctx, g.Bin, "extract", src, "--code-only", "--no-cluster", "--out", out)
	cmd.Env = append(os.Environ(), "GRAPHIFY_FORCE=1") // the snapshot may have fewer nodes after a refactor
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("graphify extract: %w: %s", err, tailText(buf.String(), 500))
	}
	n, e, err := countGraph(filepath.Join(out, "graphify-out", "graph.json"))
	if err != nil {
		return err
	}
	rs.Nodes, rs.Edges = n, e
	return nil
}

// merge joins the repos of a line into one graph and adds the gRPC links.
func (x *Indexer) merge(ctx context.Context, g *config.Graphify, project, line string, st *LineState) error {
	var parts []part
	for name, rs := range st.Repos {
		if rs.Error != "" || rs.Commit == "" {
			continue
		}
		parts = append(parts, part{name: name, graph: repoGraph(g.Dir, project, line, name), src: SrcDir(g.Dir, project, line, name)})
	}
	nodes, edges, added, err := mergeParts(ctx, g.Bin, LineDir(g.Dir, project, line), parts,
		GraphPath(g.Dir, project, line), LinksPath(g.Dir, project, line), x.logf)
	if err != nil {
		return err
	}
	st.MergedAt, st.Links, st.Nodes, st.Edges = x.now(), added, nodes, edges
	return nil
}

// repoGraph is the graph of one repo of a line.
func repoGraph(dir, project, line, repo string) string {
	return filepath.Join(outDir(dir, project, line, repo), "graphify-out", "graph.json")
}

// part is one repo of a merge: its graph and the snapshot it was built from.
type part struct{ name, graph, src string }

// mergeParts merges the repo graphs (graphify merge-graphs), scans the gRPC
// links between the snapshots and writes the linked graph and the links.
func mergeParts(ctx context.Context, bin, workDir string, parts []part, graphOut, linksOut string,
	logf func(string, ...any)) (nodes, edges, added int, err error) {
	var ok []part
	for _, p := range parts {
		if _, err := os.Stat(p.graph); err == nil {
			ok = append(ok, p)
		}
	}
	if len(ok) == 0 {
		return 0, 0, 0, errors.New("no repo graph built")
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].name < ok[j].name })
	repos := map[string]*RepoRPC{}
	var names, graphs []string
	for _, p := range ok {
		names, graphs = append(names, p.name), append(graphs, p.graph)
		r, err := ScanRepo(p.name, p.src)
		if err != nil {
			logf("graphify %s: grpc scan: %v", p.name, err)
			continue
		}
		repos[p.name] = r
	}
	links := Links(repos)
	if err := writeJSON(linksOut, links); err != nil {
		return 0, 0, 0, err
	}
	tmp := graphOut + ".merge.json"
	if len(graphs) == 1 {
		if err := copyFile(graphs[0], tmp); err != nil {
			return 0, 0, 0, err
		}
	} else {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		args := append([]string{"merge-graphs"}, graphs...)
		args = append(args, "--out", tmp)
		cmd := exec.CommandContext(cctx, bin, args...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		cmd.Dir = workDir
		if err := cmd.Run(); err != nil {
			return 0, 0, 0, fmt.Errorf("graphify merge-graphs: %w: %s", err, tailText(buf.String(), 500))
		}
	}
	defer os.Remove(tmp)
	return injectLinks(tmp, graphOut, links, len(graphs) > 1, names)
}

// injectLinks writes the merged graph with a calls_grpc edge from each
// consumer function to each provider handler of another repo.
func injectLinks(in, out string, links []Link, prefixed bool, repos []string) (nodes, edges, added int, err error) {
	b, err := os.ReadFile(in)
	if err != nil {
		return 0, 0, 0, err
	}
	var g map[string]any
	if err := json.Unmarshal(b, &g); err != nil {
		return 0, 0, 0, err
	}
	ns, _ := g["nodes"].([]any)
	key := "links"
	es, ok := g[key].([]any)
	if !ok {
		key = "edges"
		es, _ = g[key].([]any)
	}
	// file:line of a callable → node id, per repo.
	type fl struct{ repo, file string }
	index := map[fl]map[int]string{}
	for _, raw := range ns {
		n, _ := raw.(map[string]any)
		id, _ := n["id"].(string)
		file, _ := n["source_file"].(string)
		loc, _ := n["source_location"].(string)
		line, _ := strconv.Atoi(strings.TrimPrefix(loc, "L"))
		if id == "" || file == "" || line == 0 {
			continue
		}
		repo, _ := n["repo"].(string)
		if !prefixed && len(repos) == 1 {
			repo = repos[0]
		}
		k := fl{repo, filepath.ToSlash(file)}
		if index[k] == nil {
			index[k] = map[int]string{}
		}
		// A function node and its file node can share a line: keep the callable.
		if _, taken := index[k][line]; !taken || n["_callable"] == true {
			index[k][line] = id
		}
	}
	find := func(s Site) string { return index[fl{s.Repo, s.File}][s.FuncLine] }
	// Paths say which repo they belong to: graphify prints source_file in its
	// answers, and the repos share layouts (application/...).
	repoOf := map[string]string{}
	for _, raw := range ns {
		n, _ := raw.(map[string]any)
		id, _ := n["id"].(string)
		repo, _ := n["repo"].(string)
		if !prefixed && len(repos) == 1 {
			repo = repos[0]
		}
		repoOf[id] = repo
		if f, ok := n["source_file"].(string); ok && f != "" && repo != "" && !strings.HasPrefix(f, repo+"/") {
			n["source_file"] = repo + "/" + filepath.ToSlash(f)
		}
	}
	for _, raw := range es {
		e, _ := raw.(map[string]any)
		src, _ := e["source"].(string)
		if f, ok := e["source_file"].(string); ok && f != "" && repoOf[src] != "" && !strings.HasPrefix(f, repoOf[src]+"/") {
			e["source_file"] = repoOf[src] + "/" + filepath.ToSlash(f)
		}
	}
	for _, l := range links {
		for _, c := range l.Consumers {
			src := find(c)
			for _, p := range l.Providers {
				if p.Repo == c.Repo {
					continue
				}
				dst := find(p)
				if src == "" || dst == "" {
					continue
				}
				es = append(es, map[string]any{"source": src, "target": dst, "relation": "calls_grpc", "confidence": "EXTRACTED",
					"context": l.Method, "source_file": c.Repo + "/" + c.File, "source_location": "L" + strconv.Itoa(c.Line), "weight": 1.0, "_origin": "taskpilot"})
				added++
			}
		}
	}
	g[key] = es
	nb, err := json.Marshal(g)
	if err != nil {
		return 0, 0, 0, err
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, nb, 0o644); err != nil {
		return 0, 0, 0, err
	}
	return len(ns), len(es), added, os.Rename(tmp, out)
}

func countGraph(path string) (nodes, edges int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var g struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
		Links []json.RawMessage `json:"links"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return 0, 0, err
	}
	return len(g.Nodes), len(g.Edges) + len(g.Links), nil
}

// firstBranch resolves a line's branch list ("release/prod,$base": the first
// that exists on the remote; $base is the repo's base branch).
func firstBranch(ctx context.Context, repo config.Repo, spec string) (commit, branch string, err error) {
	var tried []string
	for _, b := range strings.Split(spec, ",") {
		b = strings.TrimSpace(strings.ReplaceAll(b, "$base", repo.Base))
		if b == "" {
			continue
		}
		tried = append(tried, b)
		if c, e := remoteCommit(ctx, repo, b); e == nil {
			return c, b, nil
		} else if !strings.Contains(e.Error(), "no branch") {
			err = e
		}
	}
	if err == nil {
		err = fmt.Errorf("no branch %s on the remote", strings.Join(tried, " / "))
	}
	return "", spec, err
}

// remoteCommit fetches branch from the repo's remote and returns its commit.
func remoteCommit(ctx context.Context, repo config.Repo, branch string) (string, error) {
	remote := repo.Remote
	if remote == "" {
		remote = "origin"
	}
	ref := branch
	if remote != "-" {
		fctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		_, ferr := git(fctx, repo.Path, "fetch", "--quiet", remote, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch)
		cancel()
		ref = remote + "/" + branch
		if ferr != nil {
			if strings.Contains(ferr.Error(), "couldn't find remote ref") {
				return "", fmt.Errorf("no branch %s", branch)
			}
			// Offline: use what was fetched before, if anything.
			if _, err := git(ctx, repo.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
				return "", fmt.Errorf("fetch %s: %v", branch, ferr)
			}
		}
	}
	out, err := git(ctx, repo.Path, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("no branch %s", branch)
	}
	return strings.TrimSpace(out), nil
}

// exportTree writes the files of commit into dir (git archive, no checkout:
// the repo's worktrees and branches are not touched).
func exportTree(ctx context.Context, repoPath, commit, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "archive", "--format=tar", commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exErr := untar(stdout, dir)
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%w: %s", err, tailText(stderr.String(), 300))
	}
	return exErr
}

func untar(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(root, filepath.FromSlash(h.Name))
		if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
			continue // outside the snapshot
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
		// Symlinks and the rest are skipped: a snapshot is only read.
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, tailText(errb.String(), 300))
	}
	return out.String(), nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
