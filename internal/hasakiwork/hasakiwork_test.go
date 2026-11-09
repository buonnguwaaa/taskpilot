package hasakiwork

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/db"
)

func jwt(exp time.Time) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"exp": exp.Unix(), "iat": time.Now().Unix(), "sub": 1}) + ".sig"
}

func newAuth(t *testing.T, tokenURL string) *Auth {
	t.Helper()
	d, err := db.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	a := NewAuth(d)
	a.TokenURL = tokenURL
	return a
}

func TestSetTokenValidates(t *testing.T) {
	a := newAuth(t, "")
	ctx := context.Background()
	if _, err := a.SetToken(ctx, "not-a-jwt", "op"); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := a.SetToken(ctx, jwt(time.Now().Add(-time.Minute)), "op"); err == nil {
		t.Fatal("expired token accepted")
	}
	exp, err := a.SetToken(ctx, "Bearer "+jwt(time.Now().Add(48*time.Hour)), "op")
	if err != nil || exp.Before(time.Now().Add(47*time.Hour)) {
		t.Fatalf("exp=%v err=%v", exp, err)
	}
}

func TestTokenRefreshesFromCookieNearExpiry(t *testing.T) {
	fresh := jwt(time.Now().Add(48 * time.Hour))
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		http.SetCookie(w, &http.Cookie{Name: "wshr-token", Value: "rotated", Expires: time.Now().Add(7 * 24 * time.Hour)})
		fmt.Fprintf(w, `{"token":%q}`, fresh)
	}))
	defer srv.Close()
	a := newAuth(t, srv.URL)
	ctx := context.Background()

	old := jwt(time.Now().Add(2 * time.Hour)) // inside the 6h refresh window
	if _, err := a.SetToken(ctx, old, "op"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetCookie(ctx, "Cookie: wshr-token=orig; wp.language=vi", nil, "op"); err != nil {
		t.Fatal(err)
	}
	tok, err := a.Token(ctx)
	if err != nil || tok != fresh {
		t.Fatalf("token = %q, %v; want the refreshed one", tok, err)
	}
	if gotCookie != "wshr-token=orig; wp.language=vi" {
		t.Fatalf("server got cookie %q", gotCookie)
	}
	cookie, _, _ := a.DB.Resolve(ctx, SecretCookie, "test")
	if cookie != "wshr-token=rotated; wp.language=vi" {
		t.Fatalf("rotated cookie not merged: %q", cookie)
	}
	st, _ := ReadStatus(ctx, a.DB)
	if st.CookieExpires == nil || st.CookieExpires.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("cookie expiry not taken from Set-Cookie: %+v", st.CookieExpires)
	}
}

func TestTokenFallsBackWhenRefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	a := newAuth(t, srv.URL)
	ctx := context.Background()
	a.SetCookie(ctx, "wshr-token=x", nil, "op")

	still := jwt(time.Now().Add(time.Hour))
	a.SetToken(ctx, still, "op")
	if tok, err := a.Token(ctx); err != nil || tok != still {
		t.Fatalf("should keep using a still-valid token when refresh fails: %v", err)
	}

	a.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // now it is expired
	if _, err := a.Token(ctx); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("want ErrAuthExpired, got %v", err)
	}
	st, _ := ReadStatus(ctx, a.DB)
	if !strings.Contains(st.Summary(a.Now()), "ĐÃ HẾT HẠN") {
		t.Fatalf("summary = %q", st.Summary(a.Now()))
	}
}

func TestMergeCookies(t *testing.T) {
	got := MergeCookies("a=1; b=2; c=3", []*http.Cookie{
		{Name: "b", Value: "22"},
		{Name: "c", MaxAge: -1},
		{Name: "d", Value: "4"},
	})
	if got != "a=1; b=22; d=4" {
		t.Fatalf("got %q", got)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

func TestClient(t *testing.T) {
	var lastAuth, lastLocale, lastPath string
	var form map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth, lastLocale, lastPath = r.Header.Get("Authorization"), r.Header.Get("X-Locale"), r.URL.Path
		switch {
		case r.URL.Path == "/api/hr/projects/mytasks-es":
			if r.URL.Query().Get("offset") == "0" {
				rows := make([]map[string]any, 100)
				for i := range rows {
					rows[i] = map[string]any{"id": i + 1, "name": "t", "status": 1, "project": map[string]any{"id": 8388, "name": "BOM"}}
				}
				json.NewEncoder(w).Encode(map[string]any{"status": 1, "code": 200, "message": "Success", "data": rows})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"status": 1, "code": 200, "message": "Success", "data": []any{map[string]any{"id": 101}}})
			}
		case r.URL.Path == "/api/hr/projects/task-input/7":
			io.WriteString(w, `{"message":"Success","status":1,"code":200,"data":{"id":7,"code":"HSK-1","name":"n","note":"mô tả","status":0}}`)
		case r.URL.Path == "/api/v2/task/comment" && r.Method == http.MethodGet:
			io.WriteString(w, `{"status":200,"message":"OK","data":[{"id":1,"comment":"hi"}]}`)
		case r.URL.Path == "/api/v2/task/comment" && r.Method == http.MethodPost:
			r.ParseMultipartForm(1 << 20)
			form = r.MultipartForm.Value
			io.WriteString(w, `{"status":200,"message":"OK","data":{"id":2}}`)
		case r.URL.Path == "/api/hr/projects/1":
			io.WriteString(w, `{"message":"The token has been blacklisted","status":0}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c := NewClient(staticToken("tok"))
	c.BaseURL = srv.URL + "/api"
	ctx := context.Background()

	tasks, err := c.MyTasks(ctx, "_00_01")
	if err != nil || len(tasks) != 101 || tasks[0].Project.Name != "BOM" {
		t.Fatalf("tasks=%d err=%v", len(tasks), err)
	}
	if lastAuth != "Bearer tok" || lastLocale != "vi" {
		t.Fatalf("headers: auth=%q locale=%q", lastAuth, lastLocale)
	}
	task, err := c.Task(ctx, 7)
	if err != nil || task.Note != "mô tả" || task.Code != "HSK-1" {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	comments, err := c.Comments(ctx, 7)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	if err := c.AddComment(ctx, 43149, 7, "xin chào"); err != nil {
		t.Fatal(err)
	}
	if form["obj_id"][0] != "7" || form["type"][0] != "2" || form["comment"][0] != "xin chào" || form["user_id"][0] != "43149" {
		t.Fatalf("comment form = %v", form)
	}
	if _, err := c.Task(ctx, 1); err == nil {
		t.Fatal("expected error")
	}
	var dummy any
	if err := c.do(ctx, http.MethodGet, "hr/projects/1", nil, nil, "", &dummy); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("blacklisted token must map to ErrUnauthorized, got %v (path %s)", err, lastPath)
	}
}
