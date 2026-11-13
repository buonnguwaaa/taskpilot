package scan

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestDueAndWindow(t *testing.T) {
	sc := &config.ErrorScan{Times: []string{"09:00", "14:00"}, MaxWindow: 24 * time.Hour}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 11, h, m, 0, 0, time.Local) }
	if !Due(sc, nil, at(8, 0)) {
		t.Error("never scanned: due")
	}
	last := &store.ErrorScan{StartedAt: at(9, 1), Latest: at(9, 1)}
	if Due(sc, last, at(13, 59)) || !Due(sc, last, at(14, 0)) {
		t.Error("due at the next time only")
	}
	if !Due(sc, &store.ErrorScan{StartedAt: at(8, 0).Add(-24 * time.Hour)}, at(8, 0)) {
		t.Error("before today's first time: yesterday's last time counts")
	}
	if Due(sc, &store.ErrorScan{StartedAt: at(14, 30).Add(-24 * time.Hour)}, at(8, 0)) {
		t.Error("yesterday's 14:00 scan is enough before 09:00")
	}
	failed := &store.ErrorScan{StartedAt: at(9, 0), Error: "dial tcp: i/o timeout"}
	if Due(sc, failed, at(9, 10)) || !Due(sc, failed, at(9, 15)) {
		t.Error("a failed scan is retried after RetryAfter")
	}
	from, to := Window(sc, last, at(14, 0))
	if !from.Equal(at(9, 1)) || !to.Equal(at(14, 0)) {
		t.Errorf("window since last scan = %s → %s", from, to)
	}
	from, _ = Window(sc, &store.ErrorScan{Latest: at(9, 0).Add(-72 * time.Hour)}, at(14, 0))
	if !from.Equal(at(14, 0).Add(-24 * time.Hour)) {
		t.Errorf("window is capped: %s", from)
	}
}

func TestRunKeepsSuggestions(t *testing.T) {
	var searched []string
	rows := `[{"service":"payment-zalopay","sig":"Failed to query order status <n>","count":"653","first":"1794217095","last":"1794296142","caller":"zalopay/q.go:63","sample":"Failed to query order status 1"},
	{"service":"payment-auth","sig":"failed to get permission","count":"78","first":"1794217095","last":"1794296142"},
	{"service":"payment-worker","sig":"context canceled","count":"40","first":"1794217095","last":"1794296142"},
	{"service":"payment","sig":"rare","count":"2","first":"1794217095","last":"1794296142"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			r.ParseForm()
			searched = append(searched, r.Form.Get("search")+" @"+r.Form.Get("earliest_time")+"→"+r.Form.Get("latest_time"))
			io.WriteString(w, `{"sid":"1"}`)
		case strings.HasSuffix(r.URL.Path, "/results"):
			io.WriteString(w, `{"results":`+rows+`}`)
		default:
			io.WriteString(w, `{"entry":[{"content":{"isDone":true}}]}`)
		}
	}))
	defer srv.Close()
	st, _ := store.New(t.TempDir())
	cfg := &config.Config{Projects: map[string]config.Project{"payment": {LogsProduction: &config.ProdLogs{
		Filter: "kubernetes.namespace_name=payment-prod",
		Scan:   &config.ErrorScan{Times: []string{"09:00"}, MinCount: 3, Ignore: []string{"Context Canceled"}, MaxWindow: 24 * time.Hour},
	}}}}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	s := &Scanner{Config: func() *config.Config { return cfg }, Store: st, Now: func() time.Time { return now },
		Splunk: func(context.Context) (*splunk.Client, error) {
			return &splunk.Client{BaseURL: srv.URL, Token: "t", Poll: time.Millisecond}, nil
		}}
	ctx := context.Background()

	fresh, err := s.Run(ctx, "payment")
	if err != nil || fresh != 2 {
		t.Fatalf("first scan: fresh=%d err=%v", fresh, err)
	}
	if len(searched) != 2 || !strings.Contains(searched[1], "request_id") || !strings.Contains(searched[0], "kubernetes.namespace_name=payment-prod") ||
		!strings.Contains(searched[0], "stats count") || !strings.Contains(searched[0], "@"+itoa(now.Add(-24*time.Hour).Unix())+"→"+itoa(now.Unix())) {
		t.Fatalf("search = %q", searched)
	}
	gs, _ := st.Suggestions("payment")
	if len(gs) != 2 || gs[0].Service != "payment-zalopay" || gs[0].LastCount != 653 || gs[0].Caller != "zalopay/q.go:63" {
		t.Fatalf("suggestions = %+v", gs)
	}
	// The operator dismisses one and snoozes the other.
	st.SetSuggestion(gs[1].ID, "dismissed", time.Time{}, "")
	st.SetSuggestion(gs[0].ID, "snoozed", now.Add(7*24*time.Hour), "")

	// The same errors at the next scan: counts grow, nothing comes back.
	now = now.Add(5 * time.Hour)
	if fresh, err := s.Run(ctx, "payment"); err != nil || fresh != 0 {
		t.Fatalf("second scan: fresh=%d err=%v", fresh, err)
	}
	if !strings.Contains(searched[2], "@"+itoa(now.Add(-5*time.Hour).Unix())) {
		t.Fatalf("the second scan starts where the first ended: %q", searched[2])
	}
	if gs, _ := st.Suggestions("payment"); len(gs) != 0 {
		t.Fatalf("dismissed / snoozed stay hidden: %+v", gs)
	}
	// After the snooze, a recurring error comes back.
	now = now.Add(8 * 24 * time.Hour)
	if fresh, _ := s.Run(ctx, "payment"); fresh != 1 {
		t.Fatalf("snooze over: fresh=%d", fresh)
	}
	gs, _ = st.Suggestions("payment")
	if len(gs) != 1 || gs[0].Service != "payment-zalopay" || gs[0].Count != 653*3 {
		t.Fatalf("back after snooze: %+v", gs)
	}
	last, _ := st.LastScan("payment")
	if last == nil || last.Groups != 2 || last.New != 1 {
		t.Fatalf("last scan = %+v", last)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestKeepFiltersServices(t *testing.T) {
	sc := &config.ErrorScan{MinCount: 3, Services: []string{"payment-vnpay"}}
	if keep(sc, splunk.ErrorGroup{Service: "payment-zalopay", Count: 10}) || !keep(sc, splunk.ErrorGroup{Service: "payment-vnpay", Count: 10}) {
		t.Fatal("services filter")
	}
}

// A failed scan does not move the window: the next one covers its time.
func TestFailedScanLeavesNoHole(t *testing.T) {
	st, _ := store.New(t.TempDir())
	cfg := &config.Config{Projects: map[string]config.Project{"payment": {LogsProduction: &config.ProdLogs{
		Filter: "x", Scan: &config.ErrorScan{Times: []string{"09:00"}, MinCount: 1, MaxWindow: 24 * time.Hour}}}}}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	down := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			w.WriteHeader(503)
			return
		}
		switch {
		case r.Method == http.MethodPost:
			io.WriteString(w, `{"sid":"1"}`)
		case strings.HasSuffix(r.URL.Path, "/results"):
			io.WriteString(w, `{"results":[]}`)
		default:
			io.WriteString(w, `{"entry":[{"content":{"isDone":true}}]}`)
		}
	}))
	defer srv.Close()
	s := &Scanner{Config: func() *config.Config { return cfg }, Store: st, Now: func() time.Time { return now },
		Splunk: func(context.Context) (*splunk.Client, error) {
			return &splunk.Client{BaseURL: srv.URL, Token: "t", Poll: time.Millisecond}, nil
		}}
	if _, err := s.Run(context.Background(), "payment"); err == nil {
		t.Fatal("Splunk down: the scan fails")
	}
	down, now = false, now.Add(20*time.Minute)
	if _, err := s.Run(context.Background(), "payment"); err != nil {
		t.Fatal(err)
	}
	good, _ := st.LastGoodScan("payment")
	if good == nil || !good.Earliest.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("the retry covers the failed window: %+v", good)
	}
}
