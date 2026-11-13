package runner

import (
	"testing"
	"time"
)

func TestParseQuotaReason(t *testing.T) {
	for _, s := range []string{
		"You've hit your weekly limit · resets Nov 13, 12pm (UTC)",
		"rate_limit",
		"HTTP 429 too many requests",
		"out_of_credits",
	} {
		if _, ok := ParseQuotaReason(s); !ok {
			t.Errorf("expected quota: %q", s)
		}
	}
	if _, ok := ParseQuotaReason("syntax error in file"); ok {
		t.Fatal("false positive")
	}
}

func TestObserveQuotaEvent(t *testing.T) {
	res := &Result{}
	parseEvent([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1893456000,"overageDisabledReason":"out_of_credits","isUsingOverage":false}}`), res)
	if !res.Quota.Hit || res.Quota.Resets.IsZero() || !res.Quota.Resets.Equal(time.Unix(1893456000, 0).UTC()) {
		t.Fatalf("rate_limit_event: %+v", res.Quota)
	}
	res2 := &Result{}
	parseEvent([]byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your weekly limit · resets Jul 18, 12pm (UTC)"}`), res2)
	if !res2.Quota.Hit || res2.Quota.Reason == "" {
		t.Fatalf("result 429: %+v", res2.Quota)
	}
}
