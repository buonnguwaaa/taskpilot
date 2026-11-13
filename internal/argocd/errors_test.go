package argocd

import (
	"strings"
	"testing"
)

func TestParseAndGroupErrors(t *testing.T) {
	lines := []LogLine{
		{Time: "2026-11-11T02:00:00Z", Content: `{"level":"error","message":"Failed to query order 12345","caller":"zalopay/q.go:63","prefix":"Query","request_id":"r1"}`},
		{Time: "2026-11-11T02:05:00Z", Content: `{"level":"error","message":"Failed to query order 67890","caller":"zalopay/q.go:63","request_id":"r2","error":"context deadline exceeded"}`},
		{Time: "2026-11-11T02:06:00Z", Content: `{"level":"info","message":"ok"}`},
		{Time: "2026-11-11T02:07:00Z", Content: `2026/11/11 02:07:00 [ERROR] dial tcp 10.0.0.1:6379: connection refused`},
		{Time: "2026-11-11T02:08:00Z", Content: `panic: runtime error: invalid memory address`},
		{Time: "2026-11-11T02:09:00Z", Content: `[GIN] 200 | GET /health`},
	}
	var es []Entry
	for _, l := range lines {
		es = append(es, ParseEntry("payment-qc-payment-zalopay", l))
	}
	if !es[0].IsError() || es[2].IsError() || !es[3].IsError() || !es[4].IsError() || es[5].IsError() {
		t.Fatalf("levels: %q %q %q %q %q %q", es[0].Level, es[1].Level, es[2].Level, es[3].Level, es[4].Level, es[5].Level)
	}
	if es[1].Message != "Failed to query order 67890: context deadline exceeded" {
		t.Errorf("error field joins the message: %q", es[1].Message)
	}
	gs := GroupErrors(es)
	if len(gs) != 4 {
		t.Fatalf("groups = %+v", gs)
	}
	g := gs[0] // the two "Failed to query order <n>" lines... unless the error suffix differs
	if g.Count != 1 && g.Count != 2 {
		t.Fatalf("count %d", g.Count)
	}
	var query *ErrorGroup
	for i := range gs {
		if strings.HasPrefix(gs[i].Signature, "Failed to query order <n>") && gs[i].Count == 1 && gs[i].RequestID == "r1" {
			query = &gs[i]
		}
	}
	if query == nil || query.Caller != "zalopay/q.go:63" || query.Prefix != "Query" {
		t.Fatalf("query group: %+v", gs)
	}
	if Signature("tx 0d1f2a3b-1c2d-4e5f-8a9b-0c1d2e3f4a5b id 6f1e4c2b9a8d7e6f5a4b3c2d1e count 42") != "tx <uuid> id <id> count <n>" {
		t.Errorf("signature = %q", Signature("tx 0d1f2a3b-1c2d-4e5f-8a9b-0c1d2e3f4a5b id 6f1e4c2b9a8d7e6f5a4b3c2d1e count 42"))
	}
	if ShortApp("payment-qc-payment-vnpay") != "payment-vnpay" || ShortApp("bom-api-qc") != "bom-api" {
		t.Error("ShortApp")
	}
	if got := Signature("failed to query payment status with transaction_code=HSK-T1731-20KCVKFO8, err=timeout"); got != "failed to query payment status with transaction_code=<id>, err=timeout" {
		t.Errorf("mixed code: %q", got)
	}
	// Same columns as the Production lines: time, level, service (short), message, ids.
	if f := es[0].Format(); !strings.Contains(f, " ERROR payment-zalopay ") || !strings.Contains(f, "request_id=r1") {
		t.Errorf("format = %q", f)
	}
}
