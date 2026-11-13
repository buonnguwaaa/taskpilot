package splgen

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func TestGenerate(t *testing.T) {
	var gotArgs []string
	var gotStdin string
	g := &Generator{Config: &config.Config{Claude: config.Claude{Bin: "claude", Model: "sonnet"}}, Run: func(_ context.Context, args []string, stdin string) ([]byte, error) {
		gotArgs, gotStdin = args, stdin
		return []byte(`{"is_error":false,"structured_output":{"spl":"search kubernetes.container_name=payment-techcom level=error \"refund\"","earliest":"@d","latest":"now","explanation":"lỗi refund TCB hôm nay"}}`), nil
	}}
	lc := Context{Project: "payment", Filter: "kubernetes.namespace_name=payment-prod", Services: []string{"payment-techcom"},
		Knowledge: "zap JSON: message, caller", Examples: []config.LogQuery{{Name: "Lỗi", SPL: "{filter} level=error"}}}
	r, err := g.Generate(context.Background(), lc, "refund TCB lỗi hôm nay")
	if err != nil {
		t.Fatal(err)
	}
	if r.SPL != `kubernetes.container_name=payment-techcom level=error "refund"` || r.Earliest != "@d" || r.Latest != "" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--tools ") || !strings.Contains(strings.Join(gotArgs, " "), "--model sonnet") {
		t.Fatalf("claude must run without tools: %v", gotArgs)
	}
	for _, want := range []string{"payment-techcom", "added automatically", "zap JSON", "refund TCB lỗi hôm nay", "{filter} level=error"} {
		if !strings.Contains(gotStdin, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestCheck(t *testing.T) {
	bad := []*Result{
		{SPL: ""},
		{SPL: "x | outputlookup a"},
		{SPL: "x [search y]"},
		{SPL: "x", Earliest: "yesterday"},
		{SPL: "x", Earliest: "-30d"},
		{SPL: "| inputlookup x"},
	}
	for _, r := range bad {
		if err := Check(r, 7*24*time.Hour); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
	r := &Result{SPL: " level=error ", Latest: "now"}
	if err := Check(r, 7*24*time.Hour); err != nil || r.Earliest != "-1h" || r.Latest != "" || r.SPL != "level=error" {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	if err := Check(&Result{SPL: "x", Earliest: "-1d@d", Latest: "@d"}, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestCheckUnlimited(t *testing.T) {
	for _, e := range []string{"0", "-90d", "-1y@y"} {
		err := Check(&Result{SPL: "x", Earliest: e}, 0)
		if (e == "-1y@y") != (err != nil) { // y is not a Splunk unit we accept
			t.Errorf("unlimited %s: %v", e, err)
		}
	}
	if err := Check(&Result{SPL: "x", Earliest: "0"}, 7*24*time.Hour); err == nil {
		t.Error("all time must be refused under a limit")
	}
	if err := Check(&Result{SPL: "x", Earliest: "-8d"}, 7*24*time.Hour); err == nil {
		t.Error("-8d must be refused under 7d")
	}
}
