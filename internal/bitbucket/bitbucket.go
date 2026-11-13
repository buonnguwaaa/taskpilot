// Package bitbucket is a minimal Bitbucket Cloud REST client for pull requests.
//
// Authentication is an Atlassian API token with Bitbucket scopes
// (read:repository:bitbucket, read:pullrequest:bitbucket,
// write:pullrequest:bitbucket), sent as HTTP Basic auth with the Atlassian
// account email as the username.
package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultAPIURL is the Bitbucket Cloud API.
const DefaultAPIURL = "https://api.bitbucket.org/2.0"

// Secret names in the taskpilot database.
const (
	SecretEmail = "bitbucket/email"
	SecretToken = "bitbucket/api-token"
)

// Client talks to Bitbucket Cloud.
type Client struct {
	Email   string
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client with defaults.
func New(email, token string) *Client {
	return &Client{Email: email, Token: token, BaseURL: DefaultAPIURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("bitbucket: HTTP %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Email, c.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Detail  any    `json:"detail"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
			if e.Error.Detail != nil {
				msg += fmt.Sprintf(" (%v)", e.Error.Detail)
			}
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// RepoRef identifies a repository.
type RepoRef struct{ Workspace, Slug string }

func (r RepoRef) String() string { return r.Workspace + "/" + r.Slug }

var remoteRe = regexp.MustCompile(`bitbucket\.org[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseRemote extracts workspace/slug from a git remote URL such as
// git@bitbucket.org:ws/repo.git, git@ssh.bitbucket.org:ws/repo.git or
// https://user@bitbucket.org/ws/repo.git.
func ParseRemote(remote string) (RepoRef, error) {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return RepoRef{}, fmt.Errorf("not a Bitbucket Cloud remote: %q", remote)
	}
	return RepoRef{Workspace: m[1], Slug: m[2]}, nil
}

// PullRequest is the subset of a PR taskpilot uses.
type PullRequest struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Source      Branch `json:"source"`
	Destination Branch `json:"destination"`
	Links       struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

// URL is the web link of the PR.
func (p *PullRequest) URL() string { return p.Links.HTML.Href }

// Branch is a PR endpoint.
type Branch struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
}

func branch(name string) Branch {
	var b Branch
	b.Branch.Name = name
	return b
}

// CreateInput describes a new PR.
type CreateInput struct {
	Title             string
	Description       string
	Source            string
	Destination       string
	CloseSourceBranch bool
}

// CreatePR opens a pull request.
func (c *Client) CreatePR(ctx context.Context, r RepoRef, in CreateInput) (*PullRequest, error) {
	body := map[string]any{
		"title":               in.Title,
		"description":         in.Description,
		"source":              branch(in.Source),
		"destination":         branch(in.Destination),
		"close_source_branch": in.CloseSourceBranch,
	}
	var pr PullRequest
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repositories/%s/%s/pullrequests", r.Workspace, r.Slug), body, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// FindOpenPR returns the open PR from source into dest, or nil.
func (c *Client) FindOpenPR(ctx context.Context, r RepoRef, source, dest string) (*PullRequest, error) {
	q := url.Values{"q": {fmt.Sprintf(`source.branch.name = "%s" AND destination.branch.name = "%s" AND state = "OPEN"`, source, dest)}}
	var page struct {
		Values []PullRequest `json:"values"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repositories/%s/%s/pullrequests?%s", r.Workspace, r.Slug, q.Encode()), nil, &page); err != nil {
		return nil, err
	}
	if len(page.Values) == 0 {
		return nil, nil
	}
	return &page.Values[0], nil
}

var prURLRe = regexp.MustCompile(`bitbucket\.org/([^/]+)/([^/]+)/pull-requests/(\d+)`)

// ParsePRURL extracts the repository and id from a PR web link
// (https://bitbucket.org/ws/repo/pull-requests/12).
func ParsePRURL(link string) (RepoRef, int, error) {
	m := prURLRe.FindStringSubmatch(link)
	if m == nil {
		return RepoRef{}, 0, fmt.Errorf("not a Bitbucket pull request link: %q", link)
	}
	var id int
	fmt.Sscan(m[3], &id)
	return RepoRef{Workspace: m[1], Slug: m[2]}, id, nil
}

// GetPR reads one pull request.
func (c *Client) GetPR(ctx context.Context, r RepoRef, id int) (*PullRequest, error) {
	var pr PullRequest
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repositories/%s/%s/pullrequests/%d", r.Workspace, r.Slug, id), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// MergeInput describes a merge. An empty Strategy uses the repository's
// default merge strategy.
type MergeInput struct {
	Strategy          string // merge_commit, squash or fast_forward
	Message           string
	CloseSourceBranch bool
}

// MergePR merges an open pull request. Agents reach it only through the
// merge_pull_request tool, which checks the operator asked for the merge.
// Bitbucket may finish a large merge asynchronously (HTTP 202): the returned
// PR is then nil and the merge completes on the server.
func (c *Client) MergePR(ctx context.Context, r RepoRef, id int, in MergeInput) (*PullRequest, error) {
	body := map[string]any{"close_source_branch": in.CloseSourceBranch}
	if in.Strategy != "" {
		body["merge_strategy"] = in.Strategy
	}
	if in.Message != "" {
		body["message"] = in.Message
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repositories/%s/%s/pullrequests/%d/merge", r.Workspace, r.Slug, id), body, &raw); err != nil {
		return nil, err
	}
	var pr PullRequest
	if json.Unmarshal(raw, &pr) != nil || pr.ID == 0 {
		return nil, nil
	}
	return &pr, nil
}

// DeclinePR declines (closes without merging) an open pull request.
func (c *Client) DeclinePR(ctx context.Context, r RepoRef, id int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repositories/%s/%s/pullrequests/%d/decline", r.Workspace, r.Slug, id), map[string]any{}, nil)
}

// MergeByURL merges the PR behind a web link after checking it is still open.
// done is false when Bitbucket accepted the merge but finishes it later.
func (c *Client) MergeByURL(ctx context.Context, link string, in MergeInput) (done bool, err error) {
	ref, id, err := ParsePRURL(link)
	if err != nil {
		return false, err
	}
	pr, err := c.GetPR(ctx, ref, id)
	if err != nil {
		return false, err
	}
	if pr.State != "OPEN" {
		return false, fmt.Errorf("PR #%d of %s is %s, not OPEN", id, ref, pr.State)
	}
	merged, err := c.MergePR(ctx, ref, id, in)
	if err != nil {
		return false, err
	}
	return merged != nil, nil
}

// RecentMerged returns the latest merged PRs (source and destination only).
func (c *Client) RecentMerged(ctx context.Context, r RepoRef, n int) ([]PullRequest, error) {
	q := url.Values{"state": {"MERGED"}, "pagelen": {fmt.Sprint(n)},
		"fields": {"values.source.branch.name,values.destination.branch.name"}}
	var page struct {
		Values []PullRequest `json:"values"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repositories/%s/%s/pullrequests?%s", r.Workspace, r.Slug, q.Encode()), nil, &page); err != nil {
		return nil, err
	}
	return page.Values, nil
}

var longLived = regexp.MustCompile(`^(main|master|develop|dev|release/.+|hotfix/?$|.*-qc$|.*-prod$)$`)

// IsLongLived reports whether a branch looks like an environment/release branch
// rather than a feature branch.
func IsLongLived(name string) bool { return longLived.MatchString(name) }

// FeatureDestinations counts destinations of PRs whose source is a feature
// branch, i.e. where new work lands (promotions like main→release/prod excluded).
func FeatureDestinations(prs []PullRequest) map[string]int {
	out := map[string]int{}
	for _, p := range prs {
		if !IsLongLived(p.Source.Branch.Name) {
			out[p.Destination.Branch.Name]++
		}
	}
	return out
}

// Promotions lists "source→dest" counts between long-lived branches.
func Promotions(prs []PullRequest) map[string]int {
	out := map[string]int{}
	for _, p := range prs {
		if IsLongLived(p.Source.Branch.Name) {
			out[p.Source.Branch.Name+"→"+p.Destination.Branch.Name]++
		}
	}
	return out
}

// ErrNoCredentials means no API token is configured; PRs are then opened by
// the operator from the web UI (see NewPRURL).
var ErrNoCredentials = errors.New("bitbucket API credentials are not configured")

// NewPRURL is the web page that starts a PR from source into dest, pre-filled.
// It needs no API credentials: the operator opens it in a logged-in browser.
func NewPRURL(r RepoRef, source, dest string) string {
	q := url.Values{"source": {source}, "dest": {dest}}
	return fmt.Sprintf("https://bitbucket.org/%s/%s/pull-requests/new?%s", r.Workspace, r.Slug, q.Encode())
}

// CheckAccess verifies the credentials can read the repository.
func (c *Client) CheckAccess(ctx context.Context, r RepoRef) error {
	var out struct {
		FullName string `json:"full_name"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repositories/%s/%s?fields=full_name", r.Workspace, r.Slug), nil, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
		return fmt.Errorf("%w — check the email (Atlassian login) and that the API token has Bitbucket scopes read:repository, read:pullrequest, write:pullrequest", err)
	}
	return err
}
