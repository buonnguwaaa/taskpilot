package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/workspace"
)

// GraphifyIn is the input of graphify.
type GraphifyIn struct {
	Action string `json:"action" jsonschema:"links | query | explain | path | affected | read | status"`
	Line   string `json:"line,omitempty" jsonschema:"graph to read: task (your branch: the repos you opened, with your changes, plus the others from prod), prod (release/prod) or qc (release/qc); default: task once a repo is open, prod for a production investigation, qc for a QC one"`
	Text   string `json:"text,omitempty" jsonschema:"links: filter (method, service, repo, file or function); query: the question; explain/affected: a node (name, or the id printed by an earlier answer such as bom-report::service_generateservice_generateworkorderv2)"`
	To     string `json:"to,omitempty" jsonschema:"path: the second node"`
	Depth  int    `json:"depth,omitempty" jsonschema:"query/affected: traversal depth (default 2 / 3)"`
	Repo   string `json:"repo,omitempty" jsonschema:"read: the repo"`
	File   string `json:"file,omitempty" jsonschema:"read: path inside the repo (answers print repo/path: drop the repo prefix)"`
	From   int    `json:"from_line,omitempty" jsonschema:"read: first line (default 1)"`
	Until  int    `json:"to_line,omitempty" jsonschema:"read: last line (default from_line+200)"`
}

const graphifyOut = 14000

// Graphify answers from the graphify graph of the task's project.
func (h *Handlers) Graphify(ctx context.Context, in GraphifyIn) (string, error) {
	h.graphUsed.Store(true)
	g := h.Config.Graphify
	if g == nil {
		return "", errors.New("the graphify graph is not configured (graphify in the taskpilot config): use Grep/Glob/Read")
	}
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	_ = h.Store.MarkGraphifyUsed(h.TaskID) // the hook stops reminding for this run
	lines := h.Config.ProjectGraphLines(t.Project)
	investigation := strings.Contains(t.Body, splunk.InvestigationMarker) || strings.Contains(t.Body, splunk.QCInvestigationMarker)
	hasTask := t.Workspace != "" && len(workspace.OpenedRepos(t.Workspace)) > 0
	line := strings.TrimSpace(in.Line)
	if line == "" {
		switch {
		case strings.Contains(t.Body, splunk.QCInvestigationMarker):
			line = "qc"
		case hasTask && !investigation:
			line = graphify.TaskLine // the task's own branch
		default:
			line = "prod"
		}
	}
	if in.Action == "status" {
		return graphify.Summary(h.Config, t.Project), nil
	}
	// The graph to read: a line's, or the task's own (rebuilt first if the
	// worktrees changed since: only changed files are parsed again).
	graphPath, linksPath, readLine := graphify.GraphPath(g.Dir, t.Project, line), graphify.LinksPath(g.Dir, t.Project, line), line
	switch {
	case line == graphify.TaskLine:
		if !hasTask {
			return "", fmt.Errorf("the task has no repo open yet: open_repo first, or use line prod / qc")
		}
		if _, err := graphify.BuildTask(ctx, h.Config, t.Project, t.ID, t.Workspace, nil); err != nil {
			return "", fmt.Errorf("update the task graph: %w", err)
		}
		graphPath, linksPath, readLine = graphify.TaskGraphPath(g.Dir, t.ID), graphify.TaskLinksPath(g.Dir, t.ID), graphify.TaskLine+":"+t.ID
	case !contains(lines, line):
		return "", fmt.Errorf("project %s has no code line %q; lines: %s, %s (the task's branch)", t.Project, line, strings.Join(lines, ", "), graphify.TaskLine)
	}
	if _, err := os.Stat(graphPath); err != nil {
		return "", fmt.Errorf("no graphify graph for %s/%s yet (taskpilot graphify build %s)", t.Project, line, t.Project)
	}
	var out string
	switch in.Action {
	case "links":
		ls, err := graphify.ReadLinksFile(linksPath)
		if err != nil {
			return "", fmt.Errorf("no links for %s/%s yet", t.Project, line)
		}
		out = fmt.Sprintf("gRPC calls between the repos of %s on %s:\n", t.Project, line) + graphify.FormatLinks(ls, in.Text, 40)
	case "read":
		out, err = graphify.ReadSource(g, t.Project, readLine, in.Repo, in.File, in.From, in.Until)
	case "query", "explain", "affected":
		if strings.TrimSpace(in.Text) == "" {
			return "", fmt.Errorf("%s needs text", in.Action)
		}
		out, err = graphify.QueryGraph(ctx, g.Bin, graphPath, in.Action, []string{in.Text}, in.Depth)
	case "path":
		if strings.TrimSpace(in.Text) == "" || strings.TrimSpace(in.To) == "" {
			return "", errors.New("path needs text (from) and to")
		}
		out, err = graphify.QueryGraph(ctx, g.Bin, graphPath, "path", []string{in.Text, in.To}, 0)
	default:
		return "", fmt.Errorf("unknown action %q: links, query, explain, path, affected, read or status", in.Action)
	}
	if err != nil {
		return "", err
	}
	if len(out) > graphifyOut {
		out = out[:graphifyOut] + "\n… (cut: narrow the question)"
	}
	return fmt.Sprintf("[graphify %s/%s]\n%s", t.Project, line, out), nil
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
