// Package claudeai reads claude.ai artifacts (prototypes, designs, pages
// linked from tickets) for the agent.
//
// An artifact is only served to a logged-in claude.ai user, and its content
// is rendered by the claude.ai web app inside a frame on
// <uuid>.frame.claudeusercontent.com. Headless Claude Code has no tool for
// it, so taskpilot opens the link in its dedicated Chrome profile with the
// operator's claude.ai session cookie, then reads the artifact's files from
// inside that frame (the frame lists them in _files.json) and takes a
// screenshot of what the page shows.
package claudeai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/hasakiwork"
)

const (
	// SecretSession is the store key of the claude.ai session cookie.
	SecretSession = "claude/session"
	// SessionCookieName is the claude.ai session cookie.
	SessionCookieName = "sessionKey"
	// BaseURL is claude.ai.
	BaseURL = "https://claude.ai"

	// Limits of one artifact download.
	MaxFileSize  = 10 << 20
	MaxTotalSize = 40 << 20
	MaxFiles     = 60
)

var (
	ErrNoSession    = errors.New("no claude.ai session stored: run `taskpilot auth claude`")
	ErrUnauthorized = errors.New("claude.ai rejected the session (expired or logged out): run `taskpilot auth claude` again")
)

// browserUA is sent with direct API calls: claude.ai sits behind Cloudflare,
// which is stricter with unknown clients.
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

// linkRe matches artifact links: claude.ai/artifact/<slug> and
// claude.ai/code/artifact/<uuid>, with their query (the ?sk= share key).
var linkRe = regexp.MustCompile(`https://claude\.ai/(?:code/)?artifact/[A-Za-z0-9_-]+(?:\?[^\s)\]"'<>` + "`" + `]*)?`)

// FindLinks returns the artifact links in text, without duplicates (two
// links to the same artifact count once).
func FindLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range linkRe.FindAllString(text, -1) {
		l = strings.TrimRight(l, ".,;:!?")
		id, err := Slug(l)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, l)
	}
	return out
}

// Slug returns the artifact id of a link (the last path element).
func Slug(link string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" || u.Host != "claude.ai" {
		return "", fmt.Errorf("not a claude.ai link: %s", link)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	n := len(parts)
	if n < 2 || parts[n-2] != "artifact" || parts[n-1] == "" {
		return "", fmt.Errorf("not a claude.ai artifact link: %s", link)
	}
	return parts[n-1], nil
}

// CheckSession asks claude.ai who the session belongs to.
func CheckSession(ctx context.Context, hc *http.Client, session string) (string, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/api/account", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", SessionCookieName+"="+session)
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden && strings.Contains(resp.Header.Get("Content-Type"), "json"):
		// claude.ai's own refusal; Cloudflare blocking the client answers HTML.
		return "", ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("claude.ai /api/account: HTTP %d", resp.StatusCode)
	}
	var acc struct {
		DisplayName string `json:"display_name"`
		FullName    string `json:"full_name"`
	}
	if err := json.Unmarshal(body, &acc); err != nil {
		return "", fmt.Errorf("claude.ai /api/account: %w", err)
	}
	if acc.DisplayName != "" {
		return acc.DisplayName, nil
	}
	return acc.FullName, nil
}

// Artifact is a downloaded artifact.
type Artifact struct {
	URL        string
	Title      string
	Dir        string
	Files      []string // local paths
	Skipped    []string // published files left out (too large, too many)
	Screenshot string   // local path, empty if it failed
}

// Format describes the artifact for the agent.
func (a *Artifact) Format() string {
	var b strings.Builder
	title := a.Title
	if title == "" {
		title = "(no title)"
	}
	fmt.Fprintf(&b, "claude.ai artifact %q (%s)\n", title, a.URL)
	if a.Screenshot != "" {
		fmt.Fprintf(&b, "- screenshot of what the link shows: %s\n", a.Screenshot)
	}
	for _, f := range a.Files {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	if len(a.Skipped) > 0 {
		fmt.Fprintf(&b, "- not downloaded: %s\n", strings.Join(a.Skipped, ", "))
	}
	return b.String()
}

// Fetcher downloads artifacts through the dedicated Chrome profile.
type Fetcher struct {
	Chrome  *hasakiwork.Chrome
	Session string
	// Wait bounds how long the page may take to render (default 90s).
	Wait time.Duration
}

// Fetch downloads the artifact at link into dir/claude-artifact-<slug>.
func (f *Fetcher) Fetch(ctx context.Context, link, dir string) (*Artifact, error) {
	if f.Session == "" {
		return nil, ErrNoSession
	}
	slug, err := Slug(link)
	if err != nil {
		return nil, err
	}
	wait := f.Wait
	if wait <= 0 {
		wait = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, wait+2*time.Minute)
	defer cancel()
	// An expired session still opens the page, but the artifact never shows:
	// say so now rather than after the wait. Other errors (network,
	// Cloudflare) are left to the browser, which may get through.
	if _, err := checkSession(ctx, nil, f.Session); errors.Is(err, ErrUnauthorized) {
		return nil, err
	}
	ch := f.Chrome
	launched := false
	if !ch.Running(ctx) {
		if err := ch.Launch(ctx, "about:blank"); err != nil {
			return nil, err
		}
		launched = true // Launch returns once the debug port answers
	}
	defer func() {
		if launched {
			ch.Close(context.Background())
		}
	}()
	if err := ch.SetCookie(ctx, SessionCookieName, f.Session, ".claude.ai", true); err != nil {
		return nil, err
	}
	tabID, err := ch.OpenTabID(ctx, link)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !launched {
			ch.CloseTarget(context.Background(), tabID)
		}
	}()

	var page hasakiwork.Target
	var frameUUID string
	err = waitFor(ctx, wait, func() (bool, error) {
		t, ok := target(ctx, ch, func(t hasakiwork.Target) bool { return t.ID == tabID })
		if !ok {
			return false, nil
		}
		page = t
		var st struct {
			Path  string `json:"path"`
			Frame string `json:"frame"`
		}
		if Eval(ctx, t, `({path: location.pathname, frame: document.documentElement.dataset.frameUuid || ""})`, &st) != nil {
			return false, nil
		}
		if strings.HasPrefix(st.Path, "/login") {
			return false, ErrUnauthorized
		}
		frameUUID = st.Frame
		return frameUUID != "", nil
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", link, err)
	}
	prefix := "https://" + frameUUID + ".frame.claudeusercontent.com/_f/"
	var frame hasakiwork.Target
	err = waitFor(ctx, wait, func() (bool, error) {
		t, ok := target(ctx, ch, func(t hasakiwork.Target) bool { return t.Type == "iframe" && strings.HasPrefix(t.URL, prefix) })
		frame = t
		return ok, nil
	})
	if err != nil {
		if _, cerr := checkSession(ctx, nil, f.Session); errors.Is(cerr, ErrUnauthorized) {
			return nil, cerr
		}
		return nil, fmt.Errorf("the artifact did not load (wrong link, or no access with this account?): %w", err)
	}

	var manifest struct {
		Files map[string]struct {
			Size int64 `json:"size"`
		} `json:"files"`
	}
	if err := Eval(ctx, frame, `fetch("_files.json").then(r => { if (!r.ok) throw new Error("_files.json: HTTP " + r.status); return r.json() })`, &manifest); err != nil {
		return nil, err
	}
	out := &Artifact{URL: link, Dir: filepath.Join(dir, "claude-artifact-"+slug)}
	if err := os.MkdirAll(out.Dir, 0o755); err != nil {
		return nil, err
	}
	var total int64
	for _, p := range pick(manifest.Files) {
		size := manifest.Files[p].Size
		local, ok := localPath(out.Dir, p)
		switch {
		case !ok:
			continue
		case size > MaxFileSize || total+size > MaxTotalSize || len(out.Files) >= MaxFiles:
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s (%d KB)", p, size>>10))
			continue
		}
		var b64 string
		if err := Eval(ctx, frame, fetchJS(p), &b64); err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(local, data, 0o644); err != nil {
			return nil, err
		}
		total += int64(len(data))
		out.Files = append(out.Files, local)
	}

	var title string
	Eval(ctx, page, `document.title`, &title)
	out.Title = strings.TrimSpace(title)
	// Give the frame time to draw before the screenshot.
	waitFor(ctx, 20*time.Second, func() (bool, error) {
		var state string
		Eval(ctx, frame, `document.readyState`, &state)
		return state == "complete", nil
	})
	sleep(ctx, 3*time.Second)
	if png, err := hasakiwork.Screenshot(ctx, page); err == nil && len(png) > 0 {
		p := filepath.Join(out.Dir, "screenshot.png")
		if os.WriteFile(p, png, 0o644) == nil {
			out.Screenshot = p
		}
	}
	return out, nil
}

// pick chooses the published files that are the artifact's own content: a
// canvas-type artifact (design, deck…) keeps them under project/, beside the
// type's runtime (artifact-type/, index.html, SKILL.md); a plain page is all
// its files.
func pick(files map[string]struct {
	Size int64 `json:"size"`
}) []string {
	var all, project []string
	for p := range files {
		switch {
		case strings.HasPrefix(p, "project/"):
			project = append(project, p)
		case strings.HasPrefix(p, "artifact-type/"), strings.HasPrefix(p, "_"):
		default:
			all = append(all, p)
		}
	}
	if len(project) > 0 {
		all = project
	}
	sort.Strings(all)
	return all
}

// localPath maps a published path under dir, refusing anything that would
// leave it.
func localPath(dir, p string) (string, bool) {
	clean := path.Clean("/" + p)
	if clean == "/" || strings.Contains(p, "..") {
		return "", false
	}
	return filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), true
}

func fetchJS(p string) string {
	q, _ := json.Marshal(p)
	return `(async () => {
  const r = await fetch(` + string(q) + `);
  if (!r.ok) throw new Error(` + string(q) + ` + ": HTTP " + r.status);
  const b = new Uint8Array(await r.arrayBuffer());
  let s = "";
  for (let i = 0; i < b.length; i += 32768) s += String.fromCharCode.apply(null, b.subarray(i, i + 32768));
  return btoa(s);
})()`
}

// checkSession is CheckSession, a variable so tests can stub claude.ai.
var checkSession = CheckSession

// Eval is hasakiwork.Eval, a variable so tests can stub the browser.
var Eval = hasakiwork.Eval

func target(ctx context.Context, ch *hasakiwork.Chrome, match func(hasakiwork.Target) bool) (hasakiwork.Target, bool) {
	ts, err := ch.Targets(ctx)
	if err != nil {
		return hasakiwork.Target{}, false
	}
	for _, t := range ts {
		if t.WS != "" && match(t) {
			return t, true
		}
	}
	return hasakiwork.Target{}, false
}

func waitFor(ctx context.Context, d time.Duration, ok func() (bool, error)) error {
	deadline := time.Now().Add(d)
	for {
		done, err := ok()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", d)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
