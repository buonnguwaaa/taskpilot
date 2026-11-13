// Package jenkins is a minimal, read-only Jenkins client: jobs, builds and
// console logs. Authentication is a Jenkins user and an API token (HTTP
// Basic), which needs no CSRF crumb for GET requests.
package jenkins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Secret names in the taskpilot database.
const (
	SecretUser  = "jenkins/user"
	SecretToken = "jenkins/api-token"
)

// ErrNoCredentials means `taskpilot auth jenkins` was not run.
var ErrNoCredentials = errors.New("jenkins credentials are not configured: run `taskpilot auth jenkins`")

// Client talks to one Jenkins instance.
type Client struct {
	BaseURL string
	User    string
	Token   string
	HTTP    *http.Client
}

// New returns a client with defaults.
func New(baseURL, user, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), User: user, Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	switch e.Status {
	case 401:
		return "jenkins: HTTP 401: the user or API token is wrong (run `taskpilot auth jenkins`)"
	case 403:
		return "jenkins: HTTP 403: this user cannot read it"
	case 404:
		return "jenkins: HTTP 404: no such job or build"
	}
	return fmt.Sprintf("jenkins: HTTP %d: %s", e.Status, e.Body)
}

// get reads path (relative to the base URL) into out (JSON) or returns the
// raw body when out is nil, up to limit bytes.
func (c *Client) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.User, c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &APIError{Status: resp.StatusCode, Body: msg}
	}
	return raw, err
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	raw, err := c.get(ctx, path, 32<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// JobPath is the URL path of a job from its full name ("folder/repo/branch"
// as Jenkins prints it; a multibranch branch like release/qc is the single
// segment "release%2Fqc").
func JobPath(fullName string) string {
	var b strings.Builder
	for _, seg := range strings.Split(strings.Trim(fullName, "/"), "/") {
		b.WriteString("/job/" + url.PathEscape(seg))
	}
	return b.String()
}

// FullNameFromURL turns a job or build web link into the job's full name
// and the build number (0: none).
func FullNameFromURL(base, link string) (string, int, bool) {
	p := strings.TrimPrefix(strings.TrimSpace(link), strings.TrimRight(base, "/"))
	if p == link || !strings.HasPrefix(p, "/job/") {
		return "", 0, false
	}
	var names []string
	n := 0
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i < len(segs); i++ {
		switch {
		case segs[i] == "job" && i+1 < len(segs):
			s, err := url.PathUnescape(segs[i+1])
			if err != nil {
				return "", 0, false
			}
			names = append(names, s)
			i++
		case n == 0 && isDigits(segs[i]):
			fmt.Sscan(segs[i], &n)
		}
	}
	return strings.Join(names, "/"), n, len(names) > 0
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Job is a job, folder or multibranch project.
type Job struct {
	FullName string `json:"fullName"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Class    string `json:"_class"`
	Color    string `json:"color"` // blue, red, yellow, notbuilt, disabled, aborted; *_anime: building
	Jobs     []Job  `json:"jobs"`
}

// IsFolder reports whether the job only holds other jobs.
func (j Job) IsFolder() bool {
	return j.Color == "" && len(j.Jobs) > 0 || strings.Contains(j.Class, "Folder") || strings.Contains(j.Class, "MultiBranch")
}

// Status is a readable status of the job's last build, from its color.
func (j Job) Status() string {
	c, building := strings.CutSuffix(j.Color, "_anime")
	s := map[string]string{"blue": "success", "red": "failed", "yellow": "unstable", "notbuilt": "not built",
		"disabled": "disabled", "aborted": "aborted", "grey": "pending"}[c]
	if s == "" {
		s = c
	}
	if building {
		s += ", building"
	}
	return s
}

const jobTree = "fullName,name,url,color,_class"

// Jobs lists the jobs up to depth levels of folders (multibranch projects
// and their branches included), flattened, folders left out.
func (c *Client) Jobs(ctx context.Context, depth int) ([]Job, error) {
	tree := jobTree
	for i := 0; i < max(1, depth); i++ {
		tree = jobTree + ",jobs[" + tree + "]"
	}
	var root struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.getJSON(ctx, "/api/json?tree="+url.QueryEscape("jobs["+tree+"]"), &root); err != nil {
		return nil, err
	}
	var out []Job
	var walk func([]Job)
	walk = func(js []Job) {
		for _, j := range js {
			if j.FullName == "" {
				j.FullName = j.Name
			}
			if len(j.Jobs) > 0 {
				walk(j.Jobs)
			}
			if !j.IsFolder() {
				j.Jobs = nil
				out = append(out, j)
			}
		}
	}
	walk(root.Jobs)
	return out, nil
}

// Build is one run of a job.
type Build struct {
	Number    int    `json:"number"`
	Result    string `json:"result"` // SUCCESS, FAILURE, UNSTABLE, ABORTED, NOT_BUILT; "" while building
	Building  bool   `json:"building"`
	Timestamp int64  `json:"timestamp"`
	Duration  int64  `json:"duration"`
	URL       string `json:"url"`
	Actions   []struct {
		Class      string `json:"_class"`
		Parameters []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"parameters"`
		Causes []struct {
			ShortDescription string `json:"shortDescription"`
		} `json:"causes"`
		LastBuiltRevision *struct {
			SHA1   string `json:"SHA1"`
			Branch []struct {
				Name string `json:"name"`
			} `json:"branch"`
		} `json:"lastBuiltRevision"`
	} `json:"actions"`
	ChangeSets []struct {
		Items []struct {
			CommitID string `json:"commitId"`
			Msg      string `json:"msg"`
			Author   struct {
				FullName string `json:"fullName"`
			} `json:"author"`
		} `json:"items"`
	} `json:"changeSets"`
}

const buildTree = "number,result,building,timestamp,duration,url," +
	"actions[_class,parameters[name,value],causes[shortDescription],lastBuiltRevision[SHA1,branch[name]]]," +
	"changeSets[items[commitId,msg,author[fullName]]]"

// Builds returns the latest n builds of a job, newest first.
func (c *Client) Builds(ctx context.Context, fullName string, n int) ([]Build, error) {
	var out struct {
		Builds []Build `json:"builds"`
	}
	q := url.QueryEscape(fmt.Sprintf("builds[%s]{0,%d}", buildTree, max(1, n)))
	if err := c.getJSON(ctx, JobPath(fullName)+"/api/json?tree="+q, &out); err != nil {
		return nil, err
	}
	return out.Builds, nil
}

// GetBuild reads one build (number 0: the last one).
func (c *Client) GetBuild(ctx context.Context, fullName string, number int) (*Build, error) {
	id := "lastBuild"
	if number > 0 {
		id = fmt.Sprint(number)
	}
	var b Build
	if err := c.getJSON(ctx, JobPath(fullName)+"/"+id+"/api/json?tree="+url.QueryEscape(buildTree), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// maxConsole caps how much of a console log is read.
const maxConsole = 64 << 20

// Console returns the console log of a build.
func (c *Client) Console(ctx context.Context, fullName string, number int) (string, error) {
	raw, err := c.get(ctx, JobPath(fullName)+"/"+fmt.Sprint(number)+"/consoleText", maxConsole)
	if err != nil {
		return "", err
	}
	return string(bytes.ToValidUTF8(raw, []byte("?"))), nil
}

// WhoAmI returns the authenticated user name (checks the credentials).
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	var out struct {
		Name          string `json:"name"`
		Authenticated bool   `json:"authenticated"`
	}
	if err := c.getJSON(ctx, "/me/api/json?tree=id,fullName,name", &out); err != nil {
		return "", err
	}
	return out.Name, nil
}

// Commit is the revision a build built ("" when unknown).
func (b *Build) Commit() (sha, branch string) {
	for _, a := range b.Actions {
		if r := a.LastBuiltRevision; r != nil {
			sha = r.SHA1
			if len(r.Branch) > 0 {
				branch = r.Branch[0].Name
			}
			return
		}
	}
	return
}

// Cause is why the build ran ("Started by user X", "Branch event", …).
func (b *Build) Cause() string {
	for _, a := range b.Actions {
		for _, c := range a.Causes {
			if c.ShortDescription != "" {
				return c.ShortDescription
			}
		}
	}
	return ""
}

// Params are the build parameters, "name=value".
func (b *Build) Params() []string {
	var out []string
	for _, a := range b.Actions {
		for _, p := range a.Parameters {
			out = append(out, fmt.Sprintf("%s=%v", p.Name, p.Value))
		}
	}
	return out
}

// State is the result, or "BUILDING".
func (b *Build) State() string {
	if b.Building {
		return "BUILDING"
	}
	if b.Result == "" {
		return "PENDING"
	}
	return b.Result
}

// Started is when the build started.
func (b *Build) Started() time.Time { return time.UnixMilli(b.Timestamp) }
