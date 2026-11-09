package splunk

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fake struct {
	mu       sync.Mutex
	prefix   string
	polls    int
	searches []string
	deleted  bool
	headers  http.Header
}

func (f *fake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = r.Header.Clone()
	if r.URL.Query().Get("output_mode") != "json" {
		w.WriteHeader(400)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == f.prefix+"/jobs":
		r.ParseForm()
		f.searches = append(f.searches, r.Form.Get("search")+"|"+r.Form.Get("earliest_time")+"|"+r.Form.Get("latest_time"))
		fmt.Fprint(w, `{"sid":"1700.42"}`)
	case r.Method == http.MethodGet && r.URL.Path == f.prefix+"/jobs/1700.42":
		f.polls++
		done := map[bool]string{true: "true", false: "false"}[f.polls >= 2]
		fmt.Fprintf(w, `{"entry":[{"content":{"isDone":%s,"isFailed":false}}]}`, done)
	case r.Method == http.MethodGet && r.URL.Path == f.prefix+"/jobs/1700.42/events":
		fmt.Fprint(w, `{"results":[{"_time":"2026-11-09T10:00:00","_raw":"{\"level\":\"error\",\"message\":\"boom\"}"},{"_time":"2026-11-09T10:00:01","_raw":"second"}]}`)
	case r.Method == http.MethodDelete && r.URL.Path == f.prefix+"/jobs/1700.42":
		f.deleted = true
	case strings.HasSuffix(r.URL.Path, "/authentication/current-context"):
		fmt.Fprint(w, `{"entry":[{"content":{"username":"nhannhk"}}]}`)
	default:
		w.WriteHeader(404)
	}
}

func TestSearchTokenMode(t *testing.T) {
	f := &fake{prefix: "/services/search/v2"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "tok", Poll: time.Millisecond}
	ev, err := c.Search(context.Background(), Request{SPL: "index=k8s level=error", Earliest: "-1h", MaxEvents: 10})
	if err != nil || len(ev) != 2 || f.polls < 2 || !f.deleted {
		t.Fatalf("events=%v err=%v polls=%d deleted=%v", ev, err, f.polls, f.deleted)
	}
	if f.searches[0] != "search index=k8s level=error|-1h|now" {
		t.Fatalf("job = %q", f.searches[0])
	}
	if f.headers.Get("Authorization") != "Bearer tok" {
		t.Fatalf("auth header = %q", f.headers.Get("Authorization"))
	}
	if u, err := c.WhoAmI(context.Background()); err != nil || u != "nhannhk" {
		t.Fatalf("whoami = %q %v", u, err)
	}
}

func TestSearchCookieModeUsesWebPathAndFormKey(t *testing.T) {
	f := &fake{prefix: "/en-US/splunkd/__raw/servicesNS/nobody/search/search/v2"}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Cookie: "session_id_8000=s; splunkweb_csrf_token_8000=CSRF123; splunkd_8000=d", Poll: time.Millisecond}
	if _, err := c.Search(context.Background(), Request{SPL: "x", Earliest: "-5m", MaxEvents: 5}); err != nil {
		t.Fatal(err)
	}
	if f.headers.Get("X-Splunk-Form-Key") != "CSRF123" || !strings.Contains(f.headers.Get("Cookie"), "splunkd_8000=d") ||
		f.headers.Get("Authorization") != "" {
		t.Fatalf("headers = %v", f.headers)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL, Token: "x"}).Search(context.Background(), Request{SPL: "x", Earliest: "-1h"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Search(context.Background(), Request{SPL: "x"}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no creds: %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := []string{
		`kubernetes.container_name=payment-worker level=error | head 50`,
		`index=k8s "a|b" | stats count by level | sort -count`,
		`request_id=abc | table _time, message`,
	}
	for _, q := range ok {
		if err := Validate(q); err != nil {
			t.Errorf("%q rejected: %v", q, err)
		}
	}
	bad := []string{
		`x | outputlookup evil.csv`, `x | collect index=summary`, `x | delete`, `x | sendemail to=a@b`,
		`x | script python run.py`, `x | append [search index=secret]`, `x [search index=secret | fields id]`,
		`| rest /services/authentication/users`, `x | join id [search index=hr]`, `x |  MAP search="search y"`,
	}
	for _, q := range bad {
		if err := Validate(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}

func TestLookbackAndScope(t *testing.T) {
	if d, ok := Lookback("-2d"); !ok || d != 48*time.Hour {
		t.Fatalf("-2d → %v %v", d, ok)
	}
	if d, ok := Lookback("-15m@m"); !ok || d != 15*time.Minute {
		t.Fatalf("-15m@m → %v %v", d, ok)
	}
	if _, ok := Lookback("2026-11-09T10:00:00"); ok {
		t.Fatal("absolute time is not a relative lookback")
	}
	got := Scope(`index=k8s kubernetes.namespace_name="payment-*"`, `search level=error OR level=fatal | head 5`)
	want := `(index=k8s kubernetes.namespace_name="payment-*") (level=error OR level=fatal) | head 5`
	if got != want {
		t.Fatalf("scope = %q\nwant    %q", got, want)
	}
	if got := Scope("", "a b"); got != "a b" {
		t.Fatalf("no filter = %q", got)
	}
	if got := Scope("f", `msg="x|y" z | head 1`); got != `(f) (msg="x|y" z) | head 1` {
		t.Fatalf("quoted pipe = %q", got)
	}
}

func TestFormatBounds(t *testing.T) {
	events := []Event{{"_time": "t1", "_raw": strings.Repeat("x", 2000)}, {"_time": "t2", "_raw": "y"}}
	out := Format("q", "-1h", events, 2)
	if !strings.Contains(out, "limit reached") || !strings.Contains(out, "…") || strings.Count(out, "x") > 900 {
		t.Fatalf("format = %.300s", out)
	}
}

func TestTokenExpiry(t *testing.T) {
	jwt := func(payload string) string {
		return "eyJhbGciOiJIUzUxMiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	}
	if e, ok := TokenExpiry(jwt(`{"sub":"nhannhk","exp":1800000000}`)); !ok || e == nil || e.Unix() != 1800000000 {
		t.Fatalf("exp = %v %v", e, ok)
	}
	if e, ok := TokenExpiry(jwt(`{"sub":"nhannhk"}`)); !ok || e != nil {
		t.Fatalf("no exp claim must mean never expires: %v %v", e, ok)
	}
	if _, ok := TokenExpiry("not-a-jwt"); ok {
		t.Fatal("non-JWT accepted")
	}
}

func TestCreateAndDeleteTokenViaWebSession(t *testing.T) {
	var form, deleted string
	var formKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/en-US/splunkd/__raw/services/authorization/tokens":
			r.ParseForm()
			form = r.PostForm.Encode()
			formKey = r.Header.Get("X-Splunk-Form-Key")
			fmt.Fprint(w, `{"entry":[{"content":{"id":"abc123","token":"eyJ.x.y"}}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/en-US/splunkd/__raw/services/authorization/tokens/nhannhk":
			deleted = r.URL.Query().Get("id")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	web := &Client{BaseURL: srv.URL, Cookie: "splunkweb_csrf_token_8000=K; splunkd_8000=s"}
	tok, id, err := web.CreateToken(context.Background(), "nhannhk", "taskpilot", "+90d")
	if err != nil || tok != "eyJ.x.y" || id != "abc123" {
		t.Fatalf("create = %q %q %v", tok, id, err)
	}
	if form != "audience=taskpilot&expires_on=%2B90d&name=nhannhk" || formKey != "K" {
		t.Fatalf("form = %q formKey = %q", form, formKey)
	}
	if err := web.DeleteToken(context.Background(), "nhannhk", "abc123"); err != nil || deleted != "abc123" {
		t.Fatalf("delete id=%q err=%v", deleted, err)
	}
}

func TestForbiddenIsNotReportedAsExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"messages":[{"type":"ERROR","text":"You do not have permission to create tokens"}]}`)
	}))
	defer srv.Close()
	_, _, err := (&Client{BaseURL: srv.URL, Cookie: "splunkd_8080=s"}).CreateToken(context.Background(), "u", "taskpilot", "+90d")
	if !errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) ||
		!strings.Contains(err.Error(), "edit_tokens_own") || !strings.Contains(err.Error(), "do not have permission") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaceholdersExpandAndWebURL(t *testing.T) {
	spl := `{filter} kubernetes.container_name={service} "{code}" request_id="{code}"`
	if got := strings.Join(Placeholders(spl), ","); got != "service,code" {
		t.Fatalf("placeholders = %s", got)
	}
	out, err := Expand(spl, map[string]string{"filter": "kubernetes.namespace_name=payment-prod", "service": "payment", "code": `TX"1`})
	if err != nil || out != `kubernetes.namespace_name=payment-prod kubernetes.container_name=payment "TX\"1" request_id="TX\"1"` {
		t.Fatalf("expand = %q %v", out, err)
	}
	for _, bad := range []string{"a | delete", "a [search x]", "a\nb"} {
		if _, err := Expand(`"{code}"`, map[string]string{"code": bad}); err == nil {
			t.Errorf("value %q accepted", bad)
		}
	}
	if _, err := Expand(`{code}`, map[string]string{}); err == nil {
		t.Error("missing value accepted")
	}
	u := WebURL("https://siem.hasaki.work", "en-US", "level=error", "-1h")
	if !strings.HasPrefix(u, "https://siem.hasaki.work/en-US/app/search/search?") || !strings.Contains(u, "q=search+level%3Derror") || !strings.Contains(u, "earliest=-1h") {
		t.Fatalf("url = %s", u)
	}
}

func TestPrettyReadsZapInsideFluentEnvelope(t *testing.T) {
	ev := []Event{
		{"_time": "2026-11-09T10:00:00.000+07:00", "_raw": `{"log":"{\"level\":\"error\",\"message\":\"callback timeout\",\"request_id\":\"r-1\",\"transaction_code\":\"TX1\"}\n","kubernetes":{"container_name":"payment-techcom"}}`},
		{"_time": "2026-11-09T10:00:01.000+07:00", "_raw": `{"log":"SELECT * FROM t\n","kubernetes":{"container_name":"payment"}}`},
		{"_time": "2026-11-09T10:00:02.000+07:00", "_raw": "plain text line"},
	}
	lines := Pretty(ev, 200)
	if !strings.Contains(lines[0], "ERROR") || !strings.Contains(lines[0], "payment-techcom") ||
		!strings.Contains(lines[0], "callback timeout") || !strings.Contains(lines[0], "request_id=r-1 transaction_code=TX1") {
		t.Fatalf("zap line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "SELECT * FROM t") || !strings.Contains(lines[2], "plain text line") {
		t.Fatalf("non-zap lines = %q", lines[1:])
	}
}
