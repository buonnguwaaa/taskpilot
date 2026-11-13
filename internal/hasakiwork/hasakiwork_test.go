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
	"os"
	"path/filepath"
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
	if _, err := c.Task(ctx, 1); err == nil {
		t.Fatal("expected error")
	}
	var dummy any
	if err := c.do(ctx, http.MethodGet, "hr/projects/1", nil, nil, "", &dummy); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("blacklisted token must map to ErrUnauthorized, got %v (path %s)", err, lastPath)
	}
}

func TestCreateTask(t *testing.T) {
	var got map[string]any
	var ctype string
	// Captured from the real API (2026-11-10).
	resp := `{"message":"Success","status":1,"code":200,"data":{"progress":0,"status":0,"prid":7771,"planned_hours":"480","reality_hours":0,` +
		`"name":"[BE] Scaffolding","parent_id":10297623,"milestone_id":10297539,"sub_type":0,"date_end":"2026-11-30 00:00:00","type":1,` +
		`"created_by":43149,"data":{"require_virtual_configs":true},"code":"HSK-E8HL95JP","tz":null,"id":14556670,"assign_to_user":[],"staff":[]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/hr/projects/upsert-task-input/0" {
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		ctype = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, resp)
	}))
	defer srv.Close()
	c := NewClient(staticToken("tok"))
	c.BaseURL = srv.URL + "/api"
	created, err := c.CreateTask(context.Background(), NewTask{
		Name: " [BE] Scaffolding ", ProjectID: 7771, ParentID: 10297623, MilestoneID: 10297539,
		PlannedMinutes: 480, DateEnd: time.Date(2026, 11, 30, 15, 0, 0, 0, time.Local), Note: " mô tả ",
	})
	if err != nil || created.ID != 14556670 || created.Code != "HSK-E8HL95JP" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	want := map[string]any{"id": 0.0, "name": "[BE] Scaffolding", "prid": 7771.0, "parent_id": 10297623.0, "milestone_id": 10297539.0,
		"planned_hours": 480.0, "no_assign": 1.0, "sub_type": 0.0, "date_end": "2026-11-30 00:00:00", "note": "mô tả"}
	if ctype != "application/json" || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("payload = %v (%s)", got, ctype)
	}

	resp = `{"status":1,"code":200,"message":"Success","data":{"id":5}}`
	if _, err := c.CreateTask(context.Background(), NewTask{Name: "n", ProjectID: 1}); err == nil {
		t.Error("a response without code must fail")
	}
	if _, err := c.CreateTask(context.Background(), NewTask{Name: "n"}); err == nil {
		t.Error("project is required")
	}
}

func TestDownloadFile(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("media files are public: no token must be sent")
		}
		io.WriteString(w, "PNG")
	}))
	defer srv.Close()
	c := &Client{MediaURL: srv.URL + "/production/hr/", HTTP: srv.Client()}
	dir := t.TempDir()
	for i, want := range []string{"Flow-Replace-SKU.png", "Flow-Replace-SKU-2.png"} {
		p, err := c.DownloadFile(context.Background(), "task/26/11/12/15/Flow-Replace-SKU_2026-11-12_152833.png", dir)
		if err != nil {
			t.Fatal(err)
		}
		if p != filepath.Join(dir, want) {
			t.Fatalf("download %d saved as %s, want %s", i, p, want)
		}
	}
	if got != "/production/hr/task/26/11/12/15/Flow-Replace-SKU_2026-11-12_152833.png" {
		t.Fatalf("path = %s", got)
	}
	for in, want := range map[string]string{"task/a/../../x_2026-01-02_030405.pdf": "x.pdf", "task/..": "file", "a/b.xlsx": "b.xlsx"} {
		if g := FileName(in); g != want {
			t.Errorf("FileName(%q) = %q, want %q", in, g, want)
		}
	}
}

func TestDownloadFileNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{MediaURL: srv.URL, HTTP: srv.Client()}
	dir := t.TempDir()
	if _, err := c.DownloadFile(context.Background(), "task/x.png", dir); err == nil {
		t.Fatal("404 accepted")
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("left %d files behind", len(es))
	}
}
