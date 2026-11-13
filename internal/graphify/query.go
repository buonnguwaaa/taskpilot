package graphify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// QueryTimeout bounds one graphify query.
var QueryTimeout = 90 * time.Second

// Relations followed by "affected": calls inside a repo, interface dispatch,
// and the gRPC links between repos.
var affectedRelations = []string{"calls", "indirect_call", "dispatches_to", "calls_grpc"}

// Query runs a graphify read command on the merged graph of a project's line.
//
//	query   "<question>"          BFS from the matching nodes
//	explain "<node>"              a node and its connections
//	path    "<from>" "<to>"       shortest path
//	affected "<node>"             what depends on the node (reverse)
func Query(ctx context.Context, g *config.Graphify, project, line, action string, args []string, depth int) (string, error) {
	gp := GraphPath(g.Dir, project, line)
	if _, err := os.Stat(gp); err != nil {
		return "", fmt.Errorf("no graphify graph for %s/%s yet (taskpilot graphify build %s)", project, line, project)
	}
	return QueryGraph(ctx, g.Bin, gp, action, args, depth)
}

// QueryGraph runs a graphify read command on a graph file.
func QueryGraph(ctx context.Context, bin, gp, action string, args []string, depth int) (string, error) {
	var argv []string
	switch action {
	case "query":
		if len(args) != 1 {
			return "", errors.New("query needs a question")
		}
		argv = []string{"query", args[0], "--budget", "3000"}
		if depth > 0 {
			argv = append(argv, "--depth", fmt.Sprint(min(depth, 6)))
		}
	case "explain":
		if len(args) != 1 {
			return "", errors.New("explain needs a node")
		}
		argv = []string{"explain", args[0]}
	case "path":
		if len(args) != 2 {
			return "", errors.New("path needs two nodes")
		}
		argv = []string{"path", args[0], args[1], "--undirected"}
	case "affected":
		if len(args) != 1 {
			return "", errors.New("affected needs a node")
		}
		argv = []string{"affected", args[0], "--depth", fmt.Sprint(max(1, min(depth, 5)))}
		if depth == 0 {
			argv[len(argv)-1] = "3"
		}
		for _, r := range affectedRelations {
			argv = append(argv, "--relation", r)
		}
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
	argv = append(argv, "--graph", gp)
	cctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, argv...)
	cmd.Dir = filepath.Dir(gp)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := dedupeRepo(strings.TrimSpace(out.String()))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && (ee.ExitCode() == 3 || ee.ExitCode() == 4) {
			// 3: ambiguous (the candidates are listed), 4: no match. Both are answers.
			return text, nil
		}
		if cctx.Err() != nil {
			return "", fmt.Errorf("graphify %s timed out", action)
		}
		return "", fmt.Errorf("graphify %s: %v: %s", action, err, tailText(text, 400))
	}
	return text, nil
}

// ReadSource returns lines of a file of a line's snapshot of a repo (the
// code of release/qc or release/prod, which the task's worktree may not have).
func ReadSource(g *config.Graphify, project, line, repo, file string, from, to int) (string, error) {
	if !config.ValidName(repo) {
		return "", fmt.Errorf("invalid repo %q", repo)
	}
	root := SrcDir(g.Dir, project, line, repo)
	if strings.HasPrefix(line, TaskLine+":") {
		root = taskSrc(g.Dir, strings.TrimPrefix(line, TaskLine+":"), repo)
	}
	p := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(file, "/")))
	if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("file is outside the repo")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("%s has no %s on line %s", repo, file, line)
	}
	defer f.Close()
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to-from > 400 {
		to = from + 200
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		n++
		if n < from {
			continue
		}
		if n > to {
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, sc.Text())
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%s has %d lines", file, n)
	}
	return b.String(), nil
}

// FormatLinks renders the links matching filter (method, service, repo,
// file or function; empty: all), at most limit.
func FormatLinks(links []Link, filter string, limit int) string {
	f := strings.ToLower(strings.TrimSpace(filter))
	var b strings.Builder
	n := 0
	for _, l := range links {
		if f != "" && !linkMatches(l, f) {
			continue
		}
		n++
		if n > limit {
			continue
		}
		fmt.Fprintf(&b, "%s\n", l.Method)
		for _, c := range l.Consumers {
			fmt.Fprintf(&b, "  called by %s %s:%d %s\n", c.Repo, c.File, c.Line, c.Func)
		}
		for _, p := range l.Providers {
			fmt.Fprintf(&b, "  served by %s %s:%d %s\n", p.Repo, p.File, p.Line, p.Func)
		}
	}
	if n == 0 {
		return "no gRPC link between repos matches " + fmt.Sprintf("%q", filter)
	}
	if n > limit {
		fmt.Fprintf(&b, "… %d more: narrow the filter\n", n-limit)
	}
	return b.String()
}

func linkMatches(l Link, f string) bool {
	if strings.Contains(strings.ToLower(l.Method), f) {
		return true
	}
	for _, ss := range [][]Site{l.Consumers, l.Providers} {
		for _, s := range ss {
			if strings.Contains(strings.ToLower(s.Repo+" "+s.File+" "+s.Func), f) {
				return true
			}
		}
	}
	return false
}

// Summary describes the lines of a project for the agent / operator.
func Summary(cfg *config.Config, project string) string {
	g := cfg.Graphify
	if g == nil {
		return ""
	}
	var b strings.Builder
	for _, line := range cfg.ProjectGraphLines(project) {
		st := ReadState(g.Dir, project, line)
		if st.MergedAt.IsZero() {
			fmt.Fprintf(&b, "%s: not built yet\n", line)
			continue
		}
		fmt.Fprintf(&b, "%s: %d nodes, %d gRPC edges between repos, merged %s\n", line, st.Nodes, st.Links, st.MergedAt.Local().Format("2006-01-02 15:04"))
		for _, name := range sortedKeys(st.Repos) {
			rs := st.Repos[name]
			if rs.Error != "" {
				fmt.Fprintf(&b, "  %s (%s): %s\n", name, rs.Branch, rs.Error)
				continue
			}
			fmt.Fprintf(&b, "  %s %s@%s\n", name, rs.Branch, short(rs.Commit))
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func short(c string) string {
	if len(c) > 9 {
		return c[:9]
	}
	return c
}

func sortStrings(s []string) { sort.Strings(s) }

var wordRe = regexp.MustCompile(`[\w.-]+/[\w.-]+/\S*`)

// dedupeRepo turns "bom-api/bom-api/x.go" into "bom-api/x.go": graphify
// prefixes the repo to paths when it lists candidates, and the merged graph's
// paths carry it already.
func dedupeRepo(s string) string {
	return wordRe.ReplaceAllStringFunc(s, func(w string) string {
		p := strings.SplitN(w, "/", 3)
		if len(p) == 3 && p[0] == p[1] {
			return p[0] + "/" + p[2]
		}
		return w
	})
}
