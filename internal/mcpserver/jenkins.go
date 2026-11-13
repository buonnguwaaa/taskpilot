package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/jenkins"
)

// Jenkins is read-only for agents: which jobs build the project's repos,
// their latest builds, and the console log of one build. Nothing triggers,
// stops or reconfigures a build.

// JenkinsJobsIn is the input of jenkins_jobs.
type JenkinsJobsIn struct {
	Filter string `json:"filter,omitempty" jsonschema:"only jobs whose full name contains this text (e.g. a repo or a branch such as release%2Fqc)"`
	All    bool   `json:"all,omitempty" jsonschema:"list the jobs of every project, not only this task's"`
}

// JenkinsBuildsIn is the input of jenkins_builds.
type JenkinsBuildsIn struct {
	Job   string `json:"job" jsonschema:"the job's full name as jenkins_jobs prints it (folder/repo/branch), or its web link"`
	Limit int    `json:"limit,omitempty" jsonschema:"number of latest builds (default 5, at most 20)"`
}

// JenkinsLogIn is the input of jenkins_log.
type JenkinsLogIn struct {
	Job    string `json:"job" jsonschema:"the job's full name (folder/repo/branch), or a job or build web link"`
	Build  int    `json:"build,omitempty" jsonschema:"build number; default: the last build (or the one in the link)"`
	Filter string `json:"filter,omitempty" jsonschema:"only lines containing this text (case-insensitive), with 2 lines of context; e.g. ERROR, FAIL, the name of a test"`
	Tail   int    `json:"tail,omitempty" jsonschema:"without filter: the last N lines (default 200)"`
}

const jenkinsOut = 16000

func (h *Handlers) jenkins(ctx context.Context) (*jenkins.Client, error) {
	if h.Config.Jenkins.URL == "" {
		return nil, errors.New("jenkins is not configured (jenkins.url in the taskpilot config)")
	}
	if h.Jenkins == nil {
		return nil, jenkins.ErrNoCredentials
	}
	return h.Jenkins(ctx)
}

// jobName resolves a job given as a full name or a web link, and the build
// number a link carries.
func (h *Handlers) jobName(job string) (string, int, error) {
	job = strings.TrimSpace(job)
	if job == "" {
		return "", 0, errors.New("job is required: a full name from jenkins_jobs, or a web link")
	}
	if strings.HasPrefix(job, "http") {
		name, n, ok := jenkins.FullNameFromURL(h.Config.Jenkins.URL, job)
		if !ok {
			return "", 0, fmt.Errorf("%s is not a job link of %s", job, h.Config.Jenkins.URL)
		}
		return name, n, nil
	}
	return strings.Trim(job, "/"), 0, nil
}

// JenkinsJobs lists the Jenkins jobs of the task's project.
func (h *Handlers) JenkinsJobs(ctx context.Context, in JenkinsJobsIn) (string, error) {
	cl, err := h.jenkins(ctx)
	if err != nil {
		return "", err
	}
	t, err := h.Store.LoadTask(h.TaskID)
	if err != nil {
		return "", err
	}
	p := h.Config.Projects[t.Project]
	jobs, err := cl.Jobs(ctx, h.Config.Jenkins.Depth)
	if err != nil {
		return "", err
	}
	filter := strings.ToLower(strings.TrimSpace(in.Filter))
	var b strings.Builder
	n, total := 0, 0
	for _, j := range jobs {
		if !in.All && !p.Jenkins.MatchesJob(j.FullName, p.RepoNames()) {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(j.FullName), filter) {
			continue
		}
		total++
		if b.Len() < jenkinsOut {
			fmt.Fprintf(&b, "%s  [%s]\n", j.FullName, j.Status())
			n++
		}
	}
	if total == 0 {
		if !in.All {
			return fmt.Sprintf("No Jenkins job of project %s matches (by its repo names or jenkins.jobs in the config) among %d jobs. "+
				"Try all: true with a filter.", p.Name, len(jobs)), nil
		}
		return "No Jenkins job matches.", nil
	}
	head := fmt.Sprintf("%d Jenkins job(s) (%s):\n", total, h.Config.Jenkins.URL)
	if n < total {
		head += fmt.Sprintf("(first %d; narrow with filter)\n", n)
	}
	return head + b.String() + "Pass a full name to jenkins_builds / jenkins_log. A branch with a slash is one segment: release%2Fqc.", nil
}

// JenkinsBuilds lists the latest builds of a job.
func (h *Handlers) JenkinsBuilds(ctx context.Context, in JenkinsBuildsIn) (string, error) {
	cl, err := h.jenkins(ctx)
	if err != nil {
		return "", err
	}
	name, _, err := h.jobName(in.Job)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	builds, err := cl.Builds(ctx, name, min(limit, 20))
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if len(builds) == 0 {
		return name + ": no builds yet.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — latest %d build(s):\n", name, len(builds))
	for i := range builds {
		bd := &builds[i]
		dur := time.Duration(bd.Duration) * time.Millisecond
		if bd.Building {
			dur = time.Since(bd.Started())
		}
		fmt.Fprintf(&b, "#%d %s  %s  %s", bd.Number, bd.State(), bd.Started().Local().Format("02/01 15:04"), dur.Round(time.Second))
		if sha, br := bd.Commit(); sha != "" {
			fmt.Fprintf(&b, "  %s@%.9s", br, sha)
		}
		if c := bd.Cause(); c != "" {
			fmt.Fprintf(&b, "  (%s)", c)
		}
		b.WriteString("\n")
		if ps := bd.Params(); len(ps) > 0 {
			fmt.Fprintf(&b, "    params: %s\n", strings.Join(ps, ", "))
		}
		for _, cs := range bd.ChangeSets {
			for _, it := range cs.Items {
				fmt.Fprintf(&b, "    %.9s %s — %s\n", it.CommitID, firstLineOf(it.Msg), it.Author.FullName)
			}
		}
	}
	b.WriteString("Read a build's console with jenkins_log (filter ERROR / FAIL to find the failure).")
	return b.String(), nil
}

// JenkinsLog returns part of a build's console log.
func (h *Handlers) JenkinsLog(ctx context.Context, in JenkinsLogIn) (string, error) {
	cl, err := h.jenkins(ctx)
	if err != nil {
		return "", err
	}
	name, n, err := h.jobName(in.Job)
	if err != nil {
		return "", err
	}
	if in.Build > 0 {
		n = in.Build
	}
	bd, err := cl.GetBuild(ctx, name, n)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	text, err := cl.Console(ctx, name, bd.Number)
	if err != nil {
		return "", fmt.Errorf("%s #%d: %w", name, bd.Number, err)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	maxLines := h.Config.Jenkins.MaxLogLines
	head := fmt.Sprintf("%s #%d %s (%d lines)", name, bd.Number, bd.State(), len(lines))
	var out []string
	if f := strings.ToLower(strings.TrimSpace(in.Filter)); f != "" {
		keep := make([]bool, len(lines))
		hits := 0
		for i, l := range lines {
			if strings.Contains(strings.ToLower(l), f) {
				hits++
				for k := max(0, i-2); k <= min(len(lines)-1, i+2); k++ {
					keep[k] = true
				}
			}
		}
		head += fmt.Sprintf(", %d matching %q", hits, in.Filter)
		prev := -1
		for i, k := range keep {
			if !k {
				continue
			}
			if prev >= 0 && i > prev+1 {
				out = append(out, "…")
			}
			out = append(out, fmt.Sprintf("%d: %s", i+1, lines[i]))
			prev = i
		}
		if len(out) > maxLines {
			head += fmt.Sprintf(", showing the last %d", maxLines)
			out = out[len(out)-maxLines:]
		}
	} else {
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		tail = min(tail, maxLines)
		from := max(0, len(lines)-tail)
		head += fmt.Sprintf(", lines %d-%d", from+1, len(lines))
		for i := from; i < len(lines); i++ {
			out = append(out, fmt.Sprintf("%d: %s", i+1, lines[i]))
		}
	}
	body := strings.Join(out, "\n")
	if len(body) > jenkinsOut {
		body = "…" + body[len(body)-jenkinsOut:]
	}
	return head + ":\n" + body, nil
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}
