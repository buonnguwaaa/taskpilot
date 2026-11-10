package splunk

import (
	"strings"
	"testing"
	"time"
)

func TestResolveTime(t *testing.T) {
	now := time.Date(2026, 11, 10, 16, 45, 30, 0, time.Local)
	for in, want := range map[string]string{
		"":                    "2026-11-10T16:45:30",
		"now":                 "2026-11-10T16:45:30",
		"-4h":                 "2026-11-10T12:45:30",
		"-15m":                "2026-11-10T16:30:30",
		"@d":                  "2026-11-10T00:00:00",
		"-1d@d":               "2026-11-09T00:00:00",
		"-7d@d":               "2026-11-03T00:00:00",
		"-2h@h":               "2026-11-10T14:00:00",
		"2026-11-09T08:00:00": "2026-11-09T08:00:00",
		"2026-11-09T08:00":    "2026-11-09T08:00:00",
	} {
		got, ok := ResolveTime(in, now)
		if !ok || got.Format(isoLayout) != want {
			t.Errorf("ResolveTime(%q) = %s %v, want %s", in, got.Format(isoLayout), ok, want)
		}
	}
	if _, ok := ResolveTime("yesterday", now); ok {
		t.Error("unknown forms must not resolve")
	}
	if e, l := AbsoluteWindow("-1d@d", "@d", now); e != "2026-11-09T00:00:00" || l != "2026-11-10T00:00:00" {
		t.Fatalf("yesterday = %s → %s", e, l)
	}
	if e, l := AbsoluteWindow("rt-5m", "", now); e != "rt-5m" || l != "" {
		t.Fatal("unresolvable windows are passed through")
	}
}

func TestInvestigationTicket(t *testing.T) {
	g := ErrorGroup{Service: "payment-zalopay", Signature: "Failed to query order status: Post <n>", Count: 653,
		First: time.Date(2026, 11, 10, 8, 0, 0, 0, time.Local), Last: time.Date(2026, 11, 10, 9, 0, 0, 0, time.Local),
		Caller: "zalopay/zalopay_query_order_status.go:63", Prefix: "QueryOrderStatus", RequestID: "e8b5"}
	in := Investigation{Project: "payment", Service: "payment-zalopay", Filter: "kubernetes.namespace_name=payment-prod",
		Earliest: "2026-11-10T08:00:00", Latest: "2026-11-10T10:00:00", Group: &g, SPL: "x", Samples: []string{"10:00 ERROR boom"}}
	body := in.Body()
	for _, want := range []string{InvestigationMarker, "653 lần", "zalopay_query_order_status.go:63", "QueryOrderStatus", "e8b5",
		"2026-11-10T08:00:00 → 2026-11-10T10:00:00", "kubernetes.namespace_name=payment-prod", "10:00 ERROR boom",
		"cổng plan", "complete_task"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, InvestigationMarker) {
		t.Error("the marker opens the body (the workflow skill keys on it)")
	}
	if title := in.Title(); !strings.HasPrefix(title, "Điều tra prod payment-zalopay: Failed to query") {
		t.Errorf("title = %q", title)
	}
	sym := Investigation{Project: "payment", Symptom: "Khách báo thanh toán ZaloPay treo\nchi tiết…", IDs: "TX123", Earliest: "a", Latest: "now"}
	if sym.Title() != "Điều tra prod Khách báo thanh toán ZaloPay treo" || !strings.Contains(sym.Body(), "TX123") || !strings.Contains(sym.Body(), "lúc tạo task") {
		t.Errorf("symptom ticket: %q\n%s", sym.Title(), sym.Body())
	}
}
