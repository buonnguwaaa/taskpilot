package hasakiwork

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// EpicKind is the kind of EPIC a new task goes under. Every project keeps one
// EPIC of each kind per sprint, named e.g. "[EPIC] DEV Sprint 10",
// "[EPIC] Improve Sprint 10", "[EPIC] Issue Production".
type EpicKind string

const (
	EpicDev     EpicKind = "dev"
	EpicImprove EpicKind = "improve"
	EpicIssue   EpicKind = "issue"
)

// EpicKinds lists the kinds in display order.
var EpicKinds = []EpicKind{EpicDev, EpicImprove, EpicIssue}

// Label is the kind as shown to the operator.
func (k EpicKind) Label() string {
	switch k {
	case EpicDev:
		return "DEV"
	case EpicImprove:
		return "Improve"
	case EpicIssue:
		return "Issue Production"
	}
	return string(k)
}

var (
	epicTagRe = regexp.MustCompile(`(?i)\[\s*epic\s*\]`)
	devRe     = regexp.MustCompile(`(?i)\bdevs?\b`)
)

// IsEpic reports whether a task is an EPIC (a container of tasks, named
// "[EPIC] …"), not work to do itself.
func IsEpic(name string) bool { return epicTagRe.MatchString(name) }

// ClassifyEpic tells the kind of an EPIC from its name; ok is false for a
// task that is not an EPIC or an EPIC of another kind (BA, BOMv2…).
func ClassifyEpic(name string) (EpicKind, bool) {
	if !epicTagRe.MatchString(name) {
		return "", false
	}
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "issue production"):
		return EpicIssue, true
	case strings.Contains(n, "improve"):
		return EpicImprove, true
	case devRe.MatchString(n):
		return EpicDev, true
	}
	return "", false
}

// Epic is the EPIC a new task is created under, with its sprint.
type Epic struct {
	Kind        EpicKind
	ID          int64
	Code        string
	Name        string
	MilestoneID int64
	Milestone   string
}

// ProjectTasks lists tasks of a project the current user can see (the web
// app's project view). keyword filters by name; status may be empty (all).
func (c *Client) ProjectTasks(ctx context.Context, projectID int64, status, keyword string) ([]Task, error) {
	q := url.Values{
		"option": {"all"},
		"prid":   {strconv.FormatInt(projectID, 10)},
		"limit":  {"500"},
		"offset": {"0"},
	}
	if status != "" {
		q.Set("status", status)
	}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	var out []Task
	if err := c.do(ctx, http.MethodGet, "hr/projects/mytasks-es", q, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CurrentEpics finds, for each kind, the newest open EPIC of the project.
// The search index does not return every EPIC, so candidates also come from
// the parents of the project's open tasks and of mine (the operator's tasks,
// any project; nil is fine). Each pick is checked with the task detail, which
// gives its status and sprint. Kinds without an open EPIC are missing.
func (c *Client) CurrentEpics(ctx context.Context, projectID int64, mine []Task) (map[EpicKind]*Epic, error) {
	cands := map[int64]Ref{}
	add := func(t Task) {
		if t.ProjectID != projectID {
			return
		}
		if _, ok := ClassifyEpic(t.Name); ok {
			cands[t.ID] = Ref{ID: t.ID, Name: t.Name, Code: t.Code}
		}
		if p := t.Parent; p != nil && p.ID != 0 {
			if _, ok := ClassifyEpic(p.Name); ok {
				cands[p.ID] = *p
			}
		}
	}
	for _, t := range mine {
		add(t)
	}
	epics, err := c.ProjectTasks(ctx, projectID, "", "EPIC")
	if err != nil {
		return nil, err
	}
	open, err := c.ProjectTasks(ctx, projectID, "_00_01", "")
	if err != nil {
		return nil, err
	}
	for _, t := range append(epics, open...) {
		add(t)
	}

	byKind := map[EpicKind][]Ref{}
	for _, r := range cands {
		k, _ := ClassifyEpic(r.Name)
		byKind[k] = append(byKind[k], r)
	}
	out := map[EpicKind]*Epic{}
	for k, refs := range byKind {
		sort.Slice(refs, func(i, j int) bool { return refs[i].ID > refs[j].ID }) // newest first
		for i, r := range refs {
			if i == 4 {
				break // older EPICs are closed sprints; do not walk them all
			}
			d, err := c.Task(ctx, r.ID)
			if err != nil {
				continue // e.g. no permission on that EPIC
			}
			if d.Status == 2 || d.Status == 4 { // finished, cancelled
				continue
			}
			e := &Epic{Kind: k, ID: d.ID, Code: d.Code, Name: d.Name, MilestoneID: int64(d.MilestoneID)}
			if e.Code == "" {
				e.Code = r.Code
			}
			if d.Milestone != nil {
				e.Milestone = d.Milestone.Name
				if e.MilestoneID == 0 {
					e.MilestoneID = d.Milestone.ID
				}
			}
			out[k] = e
			break
		}
	}
	return out, nil
}

// AssignStaff sets the assignees of a task, as the web app's assignee picker
// does. staff ids are Profile.StaffID values, not user ids.
func (c *Client) AssignStaff(ctx context.Context, taskID int64, staffIDs ...int64) error {
	if taskID == 0 || len(staffIDs) == 0 {
		return fmt.Errorf("assign: task and staff are required")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("id", strconv.FormatInt(taskID, 10))
	w.WriteField("field", "assign_staff")
	for _, id := range staffIDs {
		w.WriteField("value[]", strconv.FormatInt(id, 10))
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "hr/projects/mass-update-field-task-input", nil, &buf, w.FormDataContentType(), nil)
}
