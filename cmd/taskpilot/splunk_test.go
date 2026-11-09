package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

func splunkFake(t *testing.T, tokenWorks bool) (*httptest.Server, *[]string) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/services/authorization/tokens") && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"entry":[{"content":{"id":"t1","token":"tok-123"}}]}`)
		case strings.Contains(r.URL.Path, "/services/authorization/tokens/") && r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Query().Get("id"))
		case r.URL.Path == "/services/authentication/current-context":
			if !tokenWorks || r.Header.Get("Authorization") != "Bearer tok-123" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"entry":[{"content":{"username":"nhannhk"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &deleted
}

func TestCreateSplunkTokenKeepsAWorkingToken(t *testing.T) {
	srv, deleted := splunkFake(t, true)
	cfg := &config.Config{Splunk: config.Splunk{URL: srv.URL, APIURL: srv.URL}}
	web := &splunk.Client{BaseURL: srv.URL, Cookie: "splunkd_8000=s"}
	tok, _, err := createSplunkToken(context.Background(), cfg, web, "nhannhk", "+90d")
	if err != nil || tok != "tok-123" || len(*deleted) != 0 {
		t.Fatalf("tok=%q err=%v deleted=%v", tok, err, *deleted)
	}
}

func TestCreateSplunkTokenDeletesAnUnusableToken(t *testing.T) {
	srv, deleted := splunkFake(t, false)
	cfg := &config.Config{Splunk: config.Splunk{URL: srv.URL, APIURL: srv.URL}}
	web := &splunk.Client{BaseURL: srv.URL, Cookie: "splunkd_8000=s"}
	_, _, err := createSplunkToken(context.Background(), cfg, web, "nhannhk", "+90d")
	if err == nil || !strings.Contains(err.Error(), "deleted again") || len(*deleted) != 1 || (*deleted)[0] != "t1" {
		t.Fatalf("err=%v deleted=%v", err, *deleted)
	}
}
