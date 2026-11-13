package bitbucket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRemote(t *testing.T) {
	cases := map[string]string{
		"git@bitbucket.org:hasaki-tech/bom-api.git\n":            "hasaki-tech/bom-api",
		"git@ssh.bitbucket.org:hasaki-tech/payment_auth.git":     "hasaki-tech/payment_auth",
		"https://nhan@bitbucket.org/hasaki-tech/hsk-pdp-backend": "hasaki-tech/hsk-pdp-backend",
		"ssh://git@bitbucket.org/hasaki-tech/plm-base.git":       "hasaki-tech/plm-base",
	}
	for in, want := range cases {
		r, err := ParseRemote(in)
		if err != nil || r.String() != want {
			t.Errorf("%q → %v, %v; want %s", in, r, err, want)
		}
	}
	if _, err := ParseRemote("git@github.com:x/y.git"); err == nil {
		t.Error("github remote accepted")
	}
}

func TestPullRequests(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "me@hasaki.vn" || p != "tok" {
			w.WriteHeader(401)
			w.Write([]byte(`{"type":"error","error":{"message":"Unauthorized"}}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/ws/svc/pullrequests" && r.URL.Query().Get("q") != "":
			if !strings.Contains(r.URL.Query().Get("q"), `source.branch.name = "tp/old" AND destination.branch.name = "main"`) {
				w.Write([]byte(`{"values":[]}`))
				return
			}
			w.Write([]byte(`{"values":[{"id":7,"title":"old","state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/svc/pull-requests/7"}}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repositories/ws/svc/pullrequests":
			w.Write([]byte(`{"values":[{"destination":{"branch":{"name":"release/qc"}}},{"destination":{"branch":{"name":"release/qc"}}},{"destination":{"branch":{"name":"main"}}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repositories/ws/svc/pullrequests":
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(201)
			w.Write([]byte(`{"id":12,"title":"t","state":"OPEN","links":{"html":{"href":"https://bitbucket.org/ws/svc/pull-requests/12"}}}`))
		case r.URL.Path == "/repositories/ws/svc":
			w.Write([]byte(`{"full_name":"ws/svc"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"type":"error","error":{"message":"not found"}}`))
		}
	}))
	defer srv.Close()
	c := New("me@hasaki.vn", "tok")
	c.BaseURL = srv.URL
	ctx := context.Background()
	ref := RepoRef{"ws", "svc"}

	if err := c.CheckAccess(ctx, ref); err != nil {
		t.Fatal(err)
	}
	bad := New("me@hasaki.vn", "wrong")
	bad.BaseURL = srv.URL
	if err := bad.CheckAccess(ctx, ref); err == nil || !strings.Contains(err.Error(), "scopes") {
		t.Fatalf("bad token: %v", err)
	}

	pr, err := c.CreatePR(ctx, ref, CreateInput{Title: "t", Description: "d", Source: "tp/x", Destination: "main"})
	if err != nil || pr.ID != 12 || pr.URL() != "https://bitbucket.org/ws/svc/pull-requests/12" {
		t.Fatalf("create: %+v %v", pr, err)
	}
	if created["source"].(map[string]any)["branch"].(map[string]any)["name"] != "tp/x" ||
		created["destination"].(map[string]any)["branch"].(map[string]any)["name"] != "main" {
		t.Fatalf("payload = %v", created)
	}
	if got, _ := c.FindOpenPR(ctx, ref, "tp/old", "main"); got == nil || got.ID != 7 {
		t.Fatalf("find open: %+v", got)
	}
	if got, err := c.FindOpenPR(ctx, ref, "tp/none", "main"); got != nil || err != nil {
		t.Fatalf("find none: %+v %v", got, err)
	}
	if _, err := c.CreatePR(ctx, RepoRef{"ws", "nope"}, CreateInput{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("error path: %v", err)
	}
}

func TestFeatureVsPromotion(t *testing.T) {
	pr := func(src, dst string) PullRequest {
		var p PullRequest
		p.Source.Branch.Name, p.Destination.Branch.Name = src, dst
		return p
	}
	prs := []PullRequest{
		pr("feat/x", "main"), pr("fix/y--BOM_1", "main"), pr("HSK-9MA13F7B", "release/prod"),
		pr("main", "release/prod"), pr("release/qc", "release/prod"), pr("us-payment-qc", "main"),
	}
	f := FeatureDestinations(prs)
	if f["main"] != 2 || f["release/prod"] != 1 || len(f) != 2 {
		t.Fatalf("feature = %v", f)
	}
	p := Promotions(prs)
	if p["main→release/prod"] != 1 || p["release/qc→release/prod"] != 1 || p["us-payment-qc→main"] != 1 {
		t.Fatalf("promotions = %v", p)
	}
}
