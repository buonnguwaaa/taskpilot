// Package argocd is a small Argo CD REST client: application status, pod logs
// and (operator-only) sync. Auth is the argocd.token JWT that Argo CD's web UI
// sets after login, sent as a Bearer token.
package argocd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Secret name in the taskpilot database.
const SecretToken = "argocd/qc/token"

// CookieName is the session cookie Argo CD's UI uses (its value is the JWT).
const CookieName = "argocd.token"

var (
	// ErrNoCredentials means no token is stored.
	ErrNoCredentials = errors.New("argocd credentials are not configured (run `taskpilot auth argocd`)")
	// ErrUnauthorized means the token expired or was revoked.
	ErrUnauthorized = errors.New("argocd rejected the token; it expired (run `taskpilot auth argocd`)")
)

// Client talks to one Argo CD instance.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	if c.Token == "" {
		return nil, ErrNoCredentials
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = strings.NewReader(string(b))
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		var e struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && (e.Message != "" || e.Error != "") {
			msg = e.Message
			if msg == "" {
				msg = e.Error
			}
		}
		return nil, fmt.Errorf("argocd: HTTP %d: %s", resp.StatusCode, msg)
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return fmt.Errorf("argocd: unexpected response (logged out? HTML instead of JSON): %w", err)
	}
	return nil
}

// Resource is a Kubernetes object managed by an application.
type Resource struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Health    struct {
		Status string `json:"status"`
	} `json:"health"`
}

// App is the part of an Argo CD Application taskpilot shows.
type App struct {
	Name           string
	Project        string // Argo CD AppProject
	Namespace      string // destination namespace
	RepoURL        string
	Path           string
	TargetRevision string
	Health         string
	Sync           string
	Revision       string
	Images         []string
	Resources      []Resource
	OperationPhase string
	OperationMsg   string
	FinishedAt     string
	ReconciledAt   string
}

type rawApp struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Project string `json:"project"`
		Source  *struct {
			RepoURL        string `json:"repoURL"`
			Path           string `json:"path"`
			TargetRevision string `json:"targetRevision"`
		} `json:"source"`
		Sources []struct {
			RepoURL        string `json:"repoURL"`
			Path           string `json:"path"`
			TargetRevision string `json:"targetRevision"`
		} `json:"sources"`
		Destination struct {
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		Summary struct {
			Images []string `json:"images"`
		} `json:"summary"`
		Resources      []Resource `json:"resources"`
		ReconciledAt   string     `json:"reconciledAt"`
		OperationState *struct {
			Phase      string `json:"phase"`
			Message    string `json:"message"`
			FinishedAt string `json:"finishedAt"`
		} `json:"operationState"`
	} `json:"status"`
}

func (r rawApp) app() App {
	a := App{
		Name: r.Metadata.Name, Project: r.Spec.Project, Namespace: r.Spec.Destination.Namespace,
		Health: r.Status.Health.Status, Sync: r.Status.Sync.Status, Revision: r.Status.Sync.Revision,
		Images: r.Status.Summary.Images, Resources: r.Status.Resources, ReconciledAt: r.Status.ReconciledAt,
	}
	if s := r.Spec.Source; s != nil {
		a.RepoURL, a.Path, a.TargetRevision = s.RepoURL, s.Path, s.TargetRevision
	} else if len(r.Spec.Sources) > 0 {
		s := r.Spec.Sources[0]
		a.RepoURL, a.Path, a.TargetRevision = s.RepoURL, s.Path, s.TargetRevision
	}
	if op := r.Status.OperationState; op != nil {
		a.OperationPhase, a.OperationMsg, a.FinishedAt = op.Phase, op.Message, op.FinishedAt
	}
	return a
}

// Apps lists the applications the token can see.
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	var out struct {
		Items []rawApp `json:"items"`
	}
	if err := c.getJSON(ctx, "/api/v1/applications", nil, &out); err != nil {
		return nil, err
	}
	apps := make([]App, 0, len(out.Items))
	for _, it := range out.Items {
		apps = append(apps, it.app())
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return apps, nil
}

// UserInfo returns the logged-in user name.
func (c *Client) UserInfo(ctx context.Context) (string, error) {
	var out struct {
		LoggedIn bool   `json:"loggedIn"`
		Username string `json:"username"`
	}
	if err := c.getJSON(ctx, "/api/v1/session/userinfo", nil, &out); err != nil {
		return "", err
	}
	if !out.LoggedIn {
		return "", ErrUnauthorized
	}
	return out.Username, nil
}

// LogLine is one pod log line.
type LogLine struct {
	Time    string
	Pod     string
	Content string
}

// LogOptions select logs of an application's workload.
type LogOptions struct {
	Kind, Group, Name, Namespace string // workload; empty: the app's first Deployment/StatefulSet
	Container                    string
	TailLines                    int
	SinceSeconds                 int
	Filter                       string // Argo CD server-side substring filter
}

// Workload picks the main workload of an app (a Deployment named like the app,
// else the first Deployment, StatefulSet or DaemonSet).
func (a App) Workload() (Resource, bool) {
	var first *Resource
	for i, r := range a.Resources {
		switch r.Kind {
		case "Deployment", "StatefulSet", "DaemonSet", "Rollout":
			if r.Name == a.Name {
				return r, true
			}
			if first == nil {
				first = &a.Resources[i]
			}
		}
	}
	if first != nil {
		return *first, true
	}
	return Resource{}, false
}

// Logs returns the latest log lines of the app's workload (not followed).
func (c *Client) Logs(ctx context.Context, app App, o LogOptions) ([]LogLine, error) {
	if o.Kind == "" {
		w, ok := app.Workload()
		if !ok {
			return nil, fmt.Errorf("argocd: application %s has no Deployment/StatefulSet to read logs from", app.Name)
		}
		o.Kind, o.Group, o.Name, o.Namespace = w.Kind, w.Group, w.Name, w.Namespace
	}
	if o.Group == "" && (o.Kind == "Deployment" || o.Kind == "StatefulSet" || o.Kind == "DaemonSet") {
		o.Group = "apps"
	}
	if o.TailLines <= 0 {
		o.TailLines = 200
	}
	q := url.Values{
		"kind": {o.Kind}, "group": {o.Group}, "resourceName": {o.Name}, "namespace": {o.Namespace},
		"tailLines": {strconv.Itoa(o.TailLines)}, "follow": {"false"},
	}
	if o.Container != "" {
		q.Set("container", o.Container)
	}
	if o.SinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.Itoa(o.SinceSeconds))
	}
	if o.Filter != "" {
		q.Set("filter", o.Filter)
	}
	resp, err := c.request(ctx, http.MethodGet, "/api/v1/applications/"+url.PathEscape(app.Name)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var lines []LogLine
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 32<<20))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var ev struct {
			Result struct {
				Content   string `json:"content"`
				TimeStamp string `json:"timeStamp"`
				PodName   string `json:"podName"`
				Last      bool   `json:"last"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			return lines, fmt.Errorf("argocd logs: %s", ev.Error.Message)
		}
		if ev.Result.Last && ev.Result.Content == "" {
			break
		}
		lines = append(lines, LogLine{Time: ev.Result.TimeStamp, Pod: ev.Result.PodName, Content: ev.Result.Content})
	}
	return lines, sc.Err()
}

// Sync triggers a sync of the application to its target revision.
func (c *Client) Sync(ctx context.Context, name string) error {
	resp, err := c.request(ctx, http.MethodPost, "/api/v1/applications/"+url.PathEscape(name)+"/sync", nil, map[string]any{"prune": false})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AppURL is the application's page in the Argo CD UI.
func AppURL(base, name string) string {
	return strings.TrimRight(base, "/") + "/applications/" + url.PathEscape(name)
}

// TokenExpiry reads the exp claim of an Argo CD JWT (nil: no expiry; ok=false: not a JWT).
func TokenExpiry(token string) (exp *time.Time, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return nil, false
	}
	if c.Exp == 0 {
		return nil, true
	}
	t := time.Unix(c.Exp, 0).UTC()
	return &t, true
}

// Match returns the apps that belong to a taskpilot project: listed explicitly,
// in one of the project's Argo CD AppProjects, or (when matchRepos) whose name
// contains one of the project's repo names ("_" and "-" treated alike).
// An app that contains several repo names goes to the longest one, so
// "payment-gateway-global-qc" matches payment_gateway_global, not payment.
func Match(apps []App, explicit, argoProjects, repos []string, matchRepos bool, allRepos []string) []App {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", "-") }
	in := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	longest := func(name string, candidates []string) string {
		best := ""
		for _, r := range candidates {
			if strings.Contains(norm(name), norm(r)) && len(r) > len(best) {
				best = r
			}
		}
		return best
	}
	var out []App
	for _, a := range apps {
		switch {
		case in(explicit, a.Name), in(argoProjects, a.Project):
			out = append(out, a)
		case matchRepos:
			if best := longest(a.Name, allRepos); best != "" && in(repos, best) {
				out = append(out, a)
			}
		}
	}
	return out
}

// Filter keeps lines that contain needle (case-insensitive); empty keeps all.
func Filter(lines []LogLine, needle string) []LogLine {
	if needle == "" {
		return lines
	}
	n := strings.ToLower(needle)
	var out []LogLine
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l.Content), n) {
			out = append(out, l)
		}
	}
	return out
}
