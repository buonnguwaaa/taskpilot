package scan

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

func g(svc, sig string, n int) splunk.ErrorGroup {
	return splunk.ErrorGroup{Service: svc, Signature: sig, Count: n}
}

func TestFlowsFromSharedIDsAndClaude(t *testing.T) {
	groups := []splunk.ErrorGroup{
		g("payment-vnpay", "Invalid checksum", 6374),                 // 0
		g("payment-worker", "query order status via gRPC", 274),      // 1
		g("payment-zalopay", "query order status: partner", 274),     // 2
		g("payment-zalopay", "query order status from ZaloPay", 254), // 3
		g("payment-auth", "sadd wrong number of arguments", 29),      // 4
		g("payment-vnpay", "rare", 3),                                // 5
	}
	links := []splunk.Link{
		{Groups: [][2]string{{"payment-zalopay", "query order status: partner"}, {"payment-zalopay", "query order status from ZaloPay"}}, Shared: 254},
		// One shared id between big groups is chance.
		{Groups: [][2]string{{"payment-vnpay", "Invalid checksum"}, {"payment-auth", "sadd wrong number of arguments"}}, Shared: 1},
		// ...but half of a small group is not.
		{Groups: [][2]string{{"payment-vnpay", "Invalid checksum"}, {"payment-vnpay", "rare"}}, Shared: 2},
	}
	proven := linkFlows(groups, links)
	want := [][]int{{0, 5}, {2, 3}, {1}, {4}}
	if !reflect.DeepEqual(proven, want) {
		t.Fatalf("proven = %v, want %v", proven, want)
	}

	// No Claude: the proven sets.
	fs := buildFlows(groups, proven, nil)
	if len(fs) != 4 || fs[1].How != "request_id" || fs[2].How != "" {
		t.Fatalf("flows = %+v", fs)
	}

	// Claude joins the worker to the ZaloPay calls, tries to split a proven
	// set (ignored) and forgets group 4 (kept alone).
	proposed := []Flow{
		{Title: "ZaloPay: truy vấn trạng thái đơn lỗi", Cause: "ZaloPay timeout", Members: []int{1, 2}},
		{Title: "lẻ", Members: []int{3}},
		{Title: "VNPay checksum", Members: []int{0, 5, 99}},
	}
	fs = buildFlows(groups, proven, proposed)
	if len(fs) != 3 {
		t.Fatalf("flows = %+v", fs)
	}
	if fs[0].Title != "VNPay checksum" || !reflect.DeepEqual(fs[0].Members, []int{0, 5}) || fs[0].How != "request_id" {
		t.Fatalf("vnpay flow = %+v", fs[0])
	}
	if !reflect.DeepEqual(fs[1].Members, []int{1, 2, 3}) || fs[1].How != "claude" || fs[1].Title != "ZaloPay: truy vấn trạng thái đơn lỗi" || fs[1].Cause != "ZaloPay timeout" {
		t.Fatalf("zalopay flow = %+v", fs[1])
	}
	if !reflect.DeepEqual(fs[2].Members, []int{4}) {
		t.Fatalf("auth alone = %+v", fs[2])
	}
}

func TestClaudeGrouperPromptAndParse(t *testing.T) {
	var gotArgs []string
	var gotIn string
	c := &ClaudeGrouper{Bin: "claude", Run: func(_ context.Context, args []string, stdin string) ([]byte, error) {
		gotArgs, gotIn = args, stdin
		return []byte(`{"is_error":false,"structured_output":{"flows":[{"title":"A","cause":"","members":[0,1]}]}}`), nil
	}}
	fs, err := c.Group(context.Background(), GroupInput{Project: "payment", Groups: []splunk.ErrorGroup{g("a", "x", 3), g("b", "y", 3)}, Linked: [][]int{{0, 1}}})
	if err != nil || len(fs) != 1 || !reflect.DeepEqual(fs[0].Members, []int{0, 1}) {
		t.Fatalf("flows = %+v err=%v", fs, err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--tools") || !strings.Contains(gotIn, "[1] b · 3×") || !strings.Contains(gotIn, "linked") {
		t.Fatalf("args=%v in=%s", gotArgs, gotIn)
	}
}
