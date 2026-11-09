package argocd

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const appsJSON = `{"items":[
 {"metadata":{"name":"bom-api-qc"},"spec":{"project":"bom","source":{"repoURL":"git@x:bom-api","path":"k8s/qc","targetRevision":"release/qc"},"destination":{"namespace":"bom-qc"}},
  "status":{"health":{"status":"Healthy"},"sync":{"status":"Synced","revision":"abc123def4567"},"summary":{"images":["reg/bom-api:qc-1"]},
   "resources":[{"group":"","kind":"Service","name":"bom-api","namespace":"bom-qc"},{"group":"apps","kind":"Deployment","name":"bom-api-qc","namespace":"bom-qc"}],
   "operationState":{"phase":"Succeeded","message":"ok","finishedAt":"2026-11-09T09:00:00Z"}}},
 {"metadata":{"name":"payment-gateway-global-qc"},"spec":{"project":"default","sources":[{"repoURL":"r","targetRevision":"release/qc"}],"destination":{"namespace":"payment-qc"}},
  "status":{"health":{"status":"Degraded"},"sync":{"status":"OutOfSync"}}},
 {"metadata":{"name":"payment-qc"},"spec":{"project":"default"},"status":{}},
 {"metadata":{"name":"hsk-pdp-backend-qc"},"spec":{"project":"default"},"status":{}}
]}`

func server(t *testing.T) (*httptest.Server, *[]string) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case r.URL.Path == "/api/v1/applications":
			fmt.Fprint(w, appsJSON)
		case r.URL.Path == "/api/v1/session/userinfo":
			fmt.Fprint(w, `{"loggedIn":true,"username":"nhannhk"}`)
		case r.URL.Path == "/api/v1/applications/bom-api-qc/logs":
			fmt.Fprint(w, `{"result":{"content":"{\"level\":\"info\",\"msg\":\"start\"}","timeStamp":"2026-11-09T10:00:00Z","podName":"bom-api-1"}}`+"\n")
			fmt.Fprint(w, `{"result":{"content":"{\"level\":\"error\",\"msg\":\"MO-77 failed\"}","timeStamp":"2026-11-09T10:00:01Z","podName":"bom-api-1"}}`+"\n")
			fmt.Fprint(w, `{"result":{"content":"","last":true}}`+"\n")
		case r.URL.Path == "/api/v1/applications/bom-api-qc/sync" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestAppsLogsSync(t *testing.T) {
	srv, calls := server(t)
	c := &Client{BaseURL: srv.URL, Token: "tok"}
	ctx := context.Background()
	apps, err := c.Apps(ctx)
	if err != nil || len(apps) != 4 {
		t.Fatalf("apps = %d %v", len(apps), err)
	}
	a := apps[0]
	if a.Name != "bom-api-qc" || a.Health != "Healthy" || a.Sync != "Synced" || a.TargetRevision != "release/qc" ||
		a.Namespace != "bom-qc" || a.Images[0] != "reg/bom-api:qc-1" || a.OperationPhase != "Succeeded" {
		t.Fatalf("app = %+v", a)
	}
	if apps[2].TargetRevision != "release/qc" { // multi-source app
		t.Fatalf("sources not read: %+v", apps[2])
	}
	w, ok := a.Workload()
	if !ok || w.Kind != "Deployment" || w.Name != "bom-api-qc" {
		t.Fatalf("workload = %+v", w)
	}
	lines, err := c.Logs(ctx, a, LogOptions{TailLines: 50, SinceSeconds: 3600})
	if err != nil || len(lines) != 2 || lines[1].Pod != "bom-api-1" {
		t.Fatalf("logs = %+v %v", lines, err)
	}
	last := (*calls)[len(*calls)-1]
	for _, want := range []string{"kind=Deployment", "group=apps", "resourceName=bom-api-qc", "namespace=bom-qc", "tailLines=50", "sinceSeconds=3600", "follow=false"} {
		if !strings.Contains(last, want) {
			t.Errorf("logs query %q missing %s", last, want)
		}
	}
	if got := Filter(lines, "mo-77"); len(got) != 1 {
		t.Fatalf("filter = %v", got)
	}
	if err := c.Sync(ctx, "bom-api-qc"); err != nil {
		t.Fatal(err)
	}
	if u, err := c.UserInfo(ctx); err != nil || u != "nhannhk" {
		t.Fatalf("userinfo %q %v", u, err)
	}
	if _, err := (&Client{BaseURL: srv.URL, Token: "bad"}).Apps(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Apps(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := c.Logs(ctx, apps[3], LogOptions{}); err == nil {
		t.Fatal("app without a workload must error")
	}
}

func TestMatch(t *testing.T) {
	apps := []App{{Name: "bom-api-qc", Project: "x"}, {Name: "payment-gateway-global-qc"}, {Name: "payment-qc"},
		{Name: "hsk-pdp-backend-qc"}, {Name: "infra-redis", Project: "plm-infra"}, {Name: "other"}}
	all := []string{"bom-api", "payment", "payment_gateway", "payment_gateway_global", "hsk-pdp-backend"}
	names := func(as []App) string {
		var n []string
		for _, a := range as {
			n = append(n, a.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(Match(apps, nil, nil, []string{"payment", "payment_gateway"}, true, all)); got != "payment-qc" {
		t.Fatalf("payment (gateway-global belongs to the longer repo name, not to payment): %s", got)
	}
	if got := names(Match(apps, nil, []string{"plm-infra"}, []string{"hsk-pdp-backend"}, true, all)); got != "hsk-pdp-backend-qc,infra-redis" {
		t.Fatalf("plm: %s", got)
	}
	if got := names(Match(apps, []string{"other"}, nil, nil, false, all)); got != "other" {
		t.Fatalf("explicit: %s", got)
	}
}

func TestTokenExpiry(t *testing.T) {
	tok := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1800000000}`)) + ".s"
	if e, ok := TokenExpiry(tok); !ok || e.Unix() != 1800000000 {
		t.Fatalf("%v %v", e, ok)
	}
}
