package kafbat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionRedirectIsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "SESSION=good" {
			http.Redirect(w, r, "/oauth2/authorization/google", http.StatusFound)
			return
		}
		io.WriteString(w, `[{"name":"qc","status":"ONLINE","brokerCount":3}]`)
	}))
	defer srv.Close()
	ctx := context.Background()
	if _, err := (&Client{BaseURL: srv.URL, Cookie: "SESSION=old"}).Clusters(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old session: %v", err)
	}
	if _, err := (&Client{BaseURL: srv.URL}).Clusters(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no cookie: %v", err)
	}
	cs, err := (&Client{BaseURL: srv.URL, Cookie: "SESSION=good"}).Clusters(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name != "qc" {
		t.Fatalf("clusters = %+v %v", cs, err)
	}
}

func TestMessagesStream(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data:{\"type\":\"PHASE\",\"phase\":{\"name\":\"Polling\"}}\n\n"+
			"data:{\"type\":\"MESSAGE\",\"message\":{\"partition\":1,\"offset\":42,\"timestamp\":\"2026-11-12T01:02:03Z\",\"key\":\"SO1\",\"value\":\"{\\\"code\\\":\\\"SO1\\\"}\",\"headers\":{\"h\":\"v\"}}}\n\n"+
			"data:{\"type\":\"MESSAGE\",\"message\":{\"partition\":0,\"offset\":7,\"timestamp\":\"2026-11-12T01:00:00Z\",\"key\":null,\"content\":\"x\"}}\n\n"+
			"data:{\"type\":\"DONE\"}\n\n"+
			"data:{\"type\":\"MESSAGE\",\"message\":{\"partition\":0,\"offset\":8}}\n\n")
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Cookie: "SESSION=x"}
	q := MessagesQuery{Filter: "SO1", Limit: 10, Partitions: []int{1}}
	off := int64(40)
	if err := q.Start(&off, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ms, err := c.Messages(context.Background(), "qc", "payment.order", q)
	if err != nil || len(ms) != 2 || ms[0].Offset != 42 || *ms[0].Key != "SO1" || ms[1].Key != nil {
		t.Fatalf("messages = %+v %v", ms, err)
	}
	for _, want := range []string{"/api/clusters/qc/topics/payment.order/messages/v2?", "mode=FROM_OFFSET", "offset=40", "partitions=1", "stringFilter=SO1", "limit=10"} {
		if !strings.Contains(got, want) {
			t.Errorf("request %q lacks %q", got, want)
		}
	}
	out := FormatMessages(ms, 0)
	if !strings.Contains(out, `{"code":"SO1"}`) || !strings.Contains(out, "\n  x\n") {
		t.Fatalf("values (value and pre-1.5 content):\n%s", out)
	}
	if !strings.Contains(out, "#1@42") || !strings.Contains(out, "key=SO1 headers{h=v}") || !strings.Contains(out, "key=null") {
		t.Fatalf("format:\n%s", out)
	}
}

func TestStartAndSince(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	var q MessagesQuery
	if err := q.Start(nil, "2h", now); err != nil || q.Mode != "FROM_TIMESTAMP" || !q.Since.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("2h: %+v %v", q, err)
	}
	if err := (&MessagesQuery{}).Start(nil, "1d", now); err != nil {
		t.Fatal(err)
	}
	q = MessagesQuery{}
	if err := q.Start(nil, "", now); err != nil || q.Mode != "LATEST" {
		t.Fatalf("default: %+v", q)
	}
	off := int64(1)
	if err := (&MessagesQuery{}).Start(&off, "2h", now); err == nil {
		t.Fatal("offset and since together must fail")
	}
	if err := (&MessagesQuery{}).Start(nil, "yesterday-ish", now); err == nil {
		t.Fatal("bad since must fail")
	}
}

func TestAllowed(t *testing.T) {
	pats := []string{"payment-*", "*.order.*", "exact"}
	for topic, want := range map[string]bool{
		"payment-vnpay": true, "hasaki.order.created": true, "exact": true,
		"bom-api": false, "exactly": false, "": false,
	} {
		if Allowed(topic, pats) != want {
			t.Errorf("Allowed(%q) = %v", topic, !want)
		}
	}
}

func TestFormatGroupHidesOtherTopics(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	g := &Group{GroupID: "g", State: "STABLE", Partitions: []GroupPartition{
		{Topic: "payment-a", Partition: 0, CurrentOffset: n(5), EndOffset: n(9), ConsumerLag: n(4)},
		{Topic: "bom-x", Partition: 0, ConsumerLag: n(100)},
	}}
	out := FormatGroup(g, func(t string) bool { return Allowed(t, []string{"payment-*"}) })
	if strings.Contains(out, "bom-x") || !strings.Contains(out, "total lag shown: 4") || !strings.Contains(out, "1 partitions of other topics hidden") {
		t.Fatalf("format:\n%s", out)
	}
}
