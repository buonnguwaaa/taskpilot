// Package splunk runs read-only searches against Splunk.
//
// Two auth modes, as the team's own scripts do (payment/scripts/splunk-log-audit):
//   - token: Authorization: Bearer <token> on the REST API (/services/search/v2)
//   - cookie: a Splunk Web session (Cookie + X-Splunk-Form-Key from the CSRF
//     cookie) on the Web proxy path /<locale>/splunkd/__raw/servicesNS/nobody/search/search/v2
package splunk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Secret names in the taskpilot database.
const (
	SecretToken  = "splunk/token"
	SecretCookie = "splunk/session-cookie"
)

// ErrNoCredentials means neither a token nor a session cookie is stored.
var ErrNoCredentials = errors.New("splunk credentials are not configured (run `taskpilot auth splunk`)")

// ErrUnauthorized means Splunk rejected the credentials (expired session or token).
var ErrUnauthorized = errors.New("splunk rejected the credentials; the session or token expired (run `taskpilot auth splunk`)")

// ErrForbidden means the credentials are valid but the Splunk role lacks the
// capability for this call.
var ErrForbidden = errors.New("splunk: permission denied for this action")

// Client talks to one Splunk deployment.
type Client struct {
	BaseURL string // e.g. https://siem.hasaki.work
	Token   string
	Cookie  string
	Locale  string
	HTTP    *http.Client
	Poll    time.Duration
}

func (c *Client) prefix() string {
	if c.Token != "" {
		return "/services/search/v2"
	}
	return "/" + c.locale() + "/splunkd/__raw/servicesNS/nobody/search/search/v2"
}

func (c *Client) locale() string {
	if c.Locale == "" {
		return "en-US"
	}
	return c.Locale
}

var csrfRe = regexp.MustCompile(`(?:^|;\s*)splunkweb_csrf_token_\d+=([^;]+)`)

func (c *Client) do(ctx context.Context, method, path string, form url.Values, query url.Values, out any) error {
	if form == nil && method == http.MethodPost {
		form = url.Values{}
	}
	if c.Token == "" && c.Cookie == "" {
		return ErrNoCredentials
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("output_mode", "json")
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path+"?"+query.Encode(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	} else {
		req.Header.Set("Cookie", c.Cookie)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if m := csrfRe.FindStringSubmatch(c.Cookie); m != nil {
			req.Header.Set("X-Splunk-Form-Key", m[1])
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Messages []struct{ Text string } `json:"messages"`
		}
		if json.Unmarshal(raw, &e) == nil && len(e.Messages) > 0 {
			msg = e.Messages[0].Text
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w (HTTP 401)", ErrUnauthorized)
		case http.StatusForbidden:
			return fmt.Errorf("%w (HTTP 403): %s", ErrForbidden, msg)
		}
		return fmt.Errorf("splunk: HTTP %d: %s", resp.StatusCode, msg)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("splunk: unexpected response (logged out? HTML login page instead of JSON): %w", err)
	}
	return nil
}

// Event is one search result; fields are as Splunk returns them.
type Event map[string]any

// Request is a search.
type Request struct {
	SPL       string
	Earliest  string // e.g. -1h, -15m, 2026-11-09T10:00:00
	Latest    string // default now
	MaxEvents int
}

// Search creates a search job, waits for it, returns up to MaxEvents events and
// deletes the job.
func (c *Client) Search(ctx context.Context, r Request) ([]Event, error) {
	spl := strings.TrimSpace(r.SPL)
	if !strings.HasPrefix(spl, "search ") && !strings.HasPrefix(spl, "|") {
		spl = "search " + spl
	}
	latest := r.Latest
	if latest == "" {
		latest = "now"
	}
	var created struct {
		SID string `json:"sid"`
	}
	form := url.Values{"search": {spl}, "earliest_time": {r.Earliest}, "latest_time": {latest}, "exec_mode": {"normal"}}
	if err := c.do(ctx, http.MethodPost, c.prefix()+"/jobs", form, nil, &created); err != nil {
		return nil, err
	}
	if created.SID == "" {
		return nil, errors.New("splunk: no search id returned")
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.do(dctx, http.MethodDelete, c.prefix()+"/jobs/"+url.PathEscape(created.SID), nil, nil, nil)
	}()

	poll := c.Poll
	if poll == 0 {
		poll = time.Second
	}
	for {
		var st struct {
			Entry []struct {
				Content struct {
					IsDone   any `json:"isDone"`
					IsFailed any `json:"isFailed"`
					Messages any `json:"messages"`
				} `json:"content"`
			} `json:"entry"`
		}
		if err := c.do(ctx, http.MethodGet, c.prefix()+"/jobs/"+url.PathEscape(created.SID), nil, nil, &st); err != nil {
			return nil, err
		}
		if len(st.Entry) > 0 {
			ct := st.Entry[0].Content
			if truthy(ct.IsFailed) {
				return nil, fmt.Errorf("splunk: search failed: %v", ct.Messages)
			}
			if truthy(ct.IsDone) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
	var res struct {
		Results []Event `json:"results"`
	}
	// A transforming search (stats, top…) has rows only in /results; /events
	// would return the raw events it read, or nothing.
	endpoint := "/events"
	if IsTransforming(r.SPL) {
		endpoint = "/results"
	}
	q := url.Values{"count": {strconv.Itoa(r.MaxEvents)}, "offset": {"0"}, "segmentation": {"none"}}
	if err := c.do(ctx, http.MethodGet, c.prefix()+"/jobs/"+url.PathEscape(created.SID)+endpoint, nil, q, &res); err != nil {
		return nil, err
	}
	return res.Results, nil
}

// transforming are the commands whose output is a table, not events.
var transforming = map[string]bool{"stats": true, "chart": true, "timechart": true, "top": true, "rare": true,
	"table": true, "fields": false, "sistats": true, "tstats": true, "geostats": true, "xyseries": true}

// IsTransforming reports whether the search ends in a table (stats, top…):
// its rows are read from the job's results instead of its events.
func IsTransforming(spl string) bool {
	for _, m := range cmdRe.FindAllStringSubmatch(spl, -1) {
		if transforming[strings.ToLower(m[1])] {
			return true
		}
	}
	return false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	}
	return false
}

// ---- guardrails --------------------------------------------------------------

// forbidden are SPL commands that write, notify, run code or reach outside the
// search's own data. The agent's searches are read-only investigations.
var forbidden = []string{
	"outputlookup", "outputcsv", "outputtext", "collect", "mcollect", "meventcollect", "tscollect",
	"sendemail", "sendalert", "delete", "script", "run", "runshellscript", "map", "rest",
	"dbxquery", "dbxoutput", "sendresults", "loadjob", "savedsearch",
	// cross-scope combinators (would escape a project filter)
	"append", "appendcols", "appendpipe", "join", "union", "multisearch", "set", "inputlookup", "lookup",
}

var cmdRe = regexp.MustCompile(`\|\s*([A-Za-z_]+)`)

// Validate rejects searches the agent must not run.
func Validate(spl string) error {
	if strings.Contains(spl, "[") {
		return errors.New("subsearches ([...]) are not allowed")
	}
	trimmed := strings.TrimSpace(spl)
	if strings.HasPrefix(trimmed, "|") {
		return errors.New("searches must start with search terms, not a generating command (|...)")
	}
	for _, m := range cmdRe.FindAllStringSubmatch(spl, -1) {
		name := strings.ToLower(m[1])
		for _, f := range forbidden {
			if name == f {
				return fmt.Errorf("the %q command is not allowed (read-only searches only)", name)
			}
		}
	}
	return nil
}

var relRe = regexp.MustCompile(`^-(\d+)(s|m|h|d|w)(@\w+)?$`)

// Lookback returns how far back a relative earliest time reaches.
// Absolute or unrecognized times return ok=false.
func Lookback(earliest string) (time.Duration, bool) {
	m := relRe.FindStringSubmatch(strings.TrimSpace(earliest))
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, true
}

// Scope prepends a mandatory filter to the base search:
// "(filter) (base) | rest", so the filter applies whatever the base contains.
func Scope(filter, spl string) string {
	filter = strings.TrimSpace(filter)
	spl = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(spl), "search "))
	if filter == "" {
		return spl
	}
	base, rest := splitPipe(spl)
	out := "(" + filter + ")"
	if strings.TrimSpace(base) != "" {
		out += " (" + strings.TrimSpace(base) + ")"
	}
	if rest != "" {
		out += " " + rest
	}
	return out
}

// splitPipe splits at the first | outside double quotes.
func splitPipe(s string) (base, rest string) {
	inQuote := false
	for i, r := range s {
		switch {
		case r == '"' && (i == 0 || s[i-1] != '\\'):
			inQuote = !inQuote
		case r == '|' && !inQuote:
			return s[:i], s[i:]
		}
	}
	return s, ""
}

// Format renders events compactly (for the agent and the CLI), bounded in size.
func Format(query, earliest string, events []Event, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d event(s) for `%s` since %s", len(events), query, earliest)
	if len(events) == max {
		b.WriteString(" (limit reached: narrow the search or time range to see everything)")
	}
	b.WriteString("\n")
	if IsTransforming(query) {
		for _, l := range Table(events, 0) {
			b.WriteString("\n" + l)
		}
		return b.String()
	}
	const perEvent, total = 800, 48 << 10
	for _, e := range events {
		raw := fmt.Sprint(e["_raw"])
		if r := []rune(raw); len(r) > perEvent {
			raw = string(r[:perEvent]) + "…"
		}
		line := fmt.Sprintf("\n[%v] %s", e["_time"], strings.TrimSpace(raw))
		if b.Len()+len(line) > total {
			b.WriteString("\n… output truncated; narrow the search")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// servicesPath maps a /services/... REST path to the path for this auth mode
// (the Splunk Web proxy for cookie sessions).
func (c *Client) servicesPath(p string) string {
	if c.Token != "" {
		return p
	}
	return "/" + c.locale() + "/splunkd/__raw" + p
}

// CreateToken creates an authentication token for user (the logged-in user
// needs the edit_tokens_own capability). expiresOn is a Splunk time such as
// "+90d"; empty means the token never expires.
func (c *Client) CreateToken(ctx context.Context, user, audience, expiresOn string) (token, id string, err error) {
	form := url.Values{"name": {user}, "audience": {audience}}
	if expiresOn != "" {
		form.Set("expires_on", expiresOn)
	}
	var out struct {
		Entry []struct {
			Content struct {
				ID    string `json:"id"`
				Token string `json:"token"`
			} `json:"content"`
		} `json:"entry"`
	}
	if err := c.do(ctx, http.MethodPost, c.servicesPath("/services/authorization/tokens"), form, nil, &out); err != nil {
		if errors.Is(err, ErrForbidden) {
			return "", "", fmt.Errorf("%w — your Splunk role cannot create tokens (needs the edit_tokens_own capability, "+
				"and token authentication must be enabled); ask the Splunk admin", err)
		}
		return "", "", err
	}
	if len(out.Entry) == 0 || out.Entry[0].Content.Token == "" {
		return "", "", errors.New("splunk: token creation returned no token (are authentication tokens enabled?)")
	}
	return out.Entry[0].Content.Token, out.Entry[0].Content.ID, nil
}

// DeleteToken removes a token created by CreateToken.
func (c *Client) DeleteToken(ctx context.Context, user, id string) error {
	return c.do(ctx, http.MethodDelete, c.servicesPath("/services/authorization/tokens/"+url.PathEscape(user)),
		nil, url.Values{"id": {id}}, nil)
}

// WhoAmI checks the credentials and returns the Splunk username.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	path := c.servicesPath("/services/authentication/current-context")
	var out struct {
		Entry []struct {
			Content struct {
				Username string `json:"username"`
			} `json:"content"`
		} `json:"entry"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return "", err
	}
	if len(out.Entry) == 0 || out.Entry[0].Content.Username == "" {
		return "", errors.New("splunk: could not read the current user")
	}
	return out.Entry[0].Content.Username, nil
}

// TokenExpiry reads the exp claim of a Splunk authentication token (a JWT).
// ok is false when the token is not a JWT; exp is nil when it never expires.
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

// ---- saved searches -----------------------------------------------------------

var placeholderRe = regexp.MustCompile(`\{([a-z_]+)\}`)

// Placeholders lists the {name} parameters of a saved search, in order, without
// duplicates and without {filter} (filled from config).
func Placeholders(spl string) []string {
	var out []string
	seen := map[string]bool{"filter": true}
	for _, m := range placeholderRe.FindAllStringSubmatch(spl, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Expand fills placeholders. Values are escaped for use inside double quotes
// and may not contain characters that would start a new command or subsearch.
func Expand(spl string, vals map[string]string) (string, error) {
	var bad error
	out := placeholderRe.ReplaceAllStringFunc(spl, func(m string) string {
		name := m[1 : len(m)-1]
		v, ok := vals[name]
		if !ok {
			bad = fmt.Errorf("missing value for {%s}", name)
			return m
		}
		if name == "filter" {
			return v
		}
		if strings.ContainsAny(v, "|[]`\n") {
			bad = fmt.Errorf("{%s} may not contain | [ ] ` or newlines", name)
			return m
		}
		return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `"`, `\"`)
	})
	return strings.TrimSpace(out), bad
}

// WebURL opens a search in Splunk Web, up to now.
func WebURL(base, locale, spl, earliest string) string {
	return WebURLRange(base, locale, spl, earliest, "")
}

// WebURLRange opens a search in Splunk Web over [earliest, latest).
func WebURLRange(base, locale, spl, earliest, latest string) string {
	if locale == "" {
		locale = "en-US"
	}
	if latest == "" {
		latest = "now"
	}
	q := url.Values{"q": {"search " + strings.TrimPrefix(spl, "search ")}, "earliest": {earliest}, "latest": {latest}}
	return strings.TrimRight(base, "/") + "/" + locale + "/app/search/search?" + q.Encode()
}

// Table renders the rows of a transforming search (stats, top…) as aligned
// columns, internal fields left out.
func Table(rows []Event, width int) []string {
	if len(rows) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var cols []string
	for _, r := range rows {
		for k := range r {
			if !seen[k] && !strings.HasPrefix(k, "_") {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	sort.Strings(cols)
	// "count"-like numbers last reads better: put them at the end.
	sort.SliceStable(cols, func(i, j int) bool {
		return cols[i] != "count" && cols[j] == "count"
	})
	cell := func(r Event, c string) string { return strings.ReplaceAll(str(r[c]), "\n", " ") }
	wid := map[string]int{}
	for _, c := range cols {
		wid[c] = len([]rune(c))
		for _, r := range rows {
			wid[c] = min(max(wid[c], len([]rune(cell(r, c)))), 60)
		}
	}
	line := func(get func(string) string) string {
		var parts []string
		for _, c := range cols {
			v := []rune(get(c))
			if len(v) > wid[c] {
				v = append(v[:wid[c]-1], '…')
			}
			parts = append(parts, string(v)+strings.Repeat(" ", wid[c]-len(v)))
		}
		return clip(strings.TrimRight(strings.Join(parts, "  "), " "), width)
	}
	out := []string{line(func(c string) string { return c })}
	for _, r := range rows {
		r := r
		out = append(out, line(func(c string) string { return cell(r, c) }))
	}
	return out
}

// Pretty renders events for people: time, level, container, message and the
// identifiers that matter, reading zap JSON inside the Fluent Bit envelope.
func Pretty(events []Event, width int) []string {
	var out []string
	for _, e := range events {
		out = append(out, prettyOne(e, width))
	}
	return out
}

func prettyOne(e Event, width int) string {
	raw := fmt.Sprint(e["_raw"])
	var env map[string]any
	if json.Unmarshal([]byte(raw), &env) != nil {
		return clip(fmt.Sprintf("%v  %s", shortTime(e["_time"]), strings.TrimSpace(raw)), width)
	}
	app := env
	if inner, ok := env["log"].(string); ok {
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(inner)), &m) == nil {
			app = m
		} else {
			app = map[string]any{"message": strings.TrimSpace(inner)}
		}
	}
	container := ""
	if k, ok := env["kubernetes"].(map[string]any); ok {
		container, _ = k["container_name"].(string)
	}
	if container == "" {
		container, _ = env["kubernetes.container_name"].(string)
	}
	level := strings.ToUpper(str(app["level"]))
	msg := str(app["message"])
	if msg == "" {
		msg = str(app["msg"])
	}
	var extras []string
	for _, k := range []string{"request_id", "transaction_code", "refund_code", "order_code", "prefix", "error"} {
		if v := str(app[k]); v != "" {
			extras = append(extras, k+"="+v)
		}
	}
	line := fmt.Sprintf("%v %-5s %-16s %s", shortTime(e["_time"]), level, container, msg)
	if len(extras) > 0 {
		line += "  " + strings.Join(extras, " ")
	}
	return clip(line, width)
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func shortTime(v any) string {
	s := fmt.Sprint(v)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Local().Format("01-02 15:04:05")
	}
	if len(s) >= 19 {
		return strings.Replace(s[5:19], "T", " ", 1)
	}
	return s
}

func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return s
}
