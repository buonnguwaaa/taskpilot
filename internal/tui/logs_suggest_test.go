package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestSuggestionsFromScans(t *testing.T) {
	m, _ := logsModel(t, "http://127.0.0.1:1")
	st := m.d.Store
	p := m.d.Config.Projects["bom"]
	p.LogsProduction.Scan = &config.ErrorScan{Times: []string{"09:00", "14:00"}, MinCount: 3}
	m.d.Config.Projects["bom"] = p
	now := time.Now()
	for _, g := range []store.Suggestion{
		{Project: "bom", Service: "bom-api", Signature: "lock wait timeout <n>", LastCount: 12, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now, Caller: "mo/repo.go:88"},
		{Project: "bom", Service: "bom-worker", Signature: "kafka publish failed", LastCount: 5, FirstSeen: now.Add(-time.Hour), LastSeen: now},
	} {
		g := g
		st.UpsertSuggestion(&g, 1, now)
	}
	m, _ = press(t, m, "3")
	if v := m.View(); !strings.Contains(v, itemSuggest+" (2)") {
		t.Fatalf("the suggestions entry with its count:\n%s", v)
	}
	m, _ = press(t, m, "enter") // the first entry
	if !m.logs.suggFocus || len(m.logs.flows) != 2 {
		t.Fatalf("suggestions open: %+v", m.logs.flows)
	}
	if v := m.View(); !strings.Contains(v, "Đề xuất sửa lỗi — bom") || !strings.Contains(v, "lịch 09:00, 14:00") || !strings.Contains(v, "x bỏ qua hẳn") {
		t.Fatalf("view:\n%s", v)
	}
	// x: never again.
	m, _ = press(t, m, "j", "x")
	if gs, _ := st.Suggestions("bom"); len(gs) != 1 || gs[0].Service != "bom-api" {
		t.Fatalf("after x: %+v", gs)
	}
	// n opens the investigation; the suggestion is marked only once the task is submitted.
	m, _ = press(t, m, "n")
	if m.modal == nil || !strings.Contains(m.modal.value("body"), splunk.InvestigationMarker) || !strings.Contains(m.modal.value("body"), "12 lần") {
		t.Fatalf("investigation form: %+v", m.modal)
	}
	m, _ = press(t, m, "esc")
	if gs, _ := st.Suggestions("bom"); len(gs) != 1 {
		t.Fatal("cancelling the form keeps the suggestion")
	}
	m, _ = press(t, m, "n")
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("submit: %s", m.modal.err)
	}
	if gs, _ := st.Suggestions("bom"); len(gs) != 0 {
		t.Fatal("a submitted task closes the suggestion")
	}
	m.logs.suggestions = m.suggestionCount("")
	if strings.Contains(m.tabLabel(tabLogs), "đề xuất") {
		t.Fatal("no waiting suggestion: plain tab label")
	}
}

func TestSuggestionFlows(t *testing.T) {
	m, _ := logsModel(t, "http://127.0.0.1:1")
	st := m.d.Store
	now := time.Now()
	var ids []int64
	for _, g := range []store.Suggestion{
		{Project: "bom", Service: "bom-worker", Signature: "query order status via gRPC", LastCount: 274, FirstSeen: now.Add(-time.Hour), LastSeen: now},
		{Project: "bom", Service: "bom-api", Signature: "partner timeout", LastCount: 274, FirstSeen: now.Add(-time.Hour), LastSeen: now, Caller: "p/q.go:63"},
		{Project: "bom", Service: "bom-api", Signature: "lock wait timeout", LastCount: 12, FirstSeen: now.Add(-time.Hour), LastSeen: now},
	} {
		g := g
		st.UpsertSuggestion(&g, 1, now)
		ids = append(ids, g.ID)
	}
	if err := st.SaveFlow("bom", 1, "Partner: truy vấn đơn timeout", "partner chậm", "claude", ids[:2]); err != nil {
		t.Fatal(err)
	}
	m, _ = press(t, m, "3", "enter")
	v := m.View()
	if len(m.logs.flows) != 2 || !strings.Contains(v, "2 luồng · 3 nhóm") || !strings.Contains(v, "Partner: truy vấn đơn timeout") ||
		!strings.Contains(v, "Nguyên nhân có thể: partner chậm") {
		t.Fatalf("flows view:\n%s", v)
	}
	// Enter opens the flow; its groups are rows.
	m, _ = press(t, m, "enter")
	if len(m.suggEntries()) != 4 {
		t.Fatalf("entries = %d", len(m.suggEntries()))
	}
	m, _ = press(t, m, "k") // back on the flow
	// f: one task for the whole flow, with the other group in it.
	m, _ = press(t, m, "f")
	body := m.modal.value("body")
	if !strings.Contains(m.modal.value("title"), "Sửa luồng lỗi prod: Partner") || !strings.Contains(body, "## Cùng luồng lỗi") ||
		!strings.Contains(body, "partner timeout") {
		t.Fatalf("fix form: %s\n%s", m.modal.value("title"), body)
	}
	m, _ = press(t, m, "ctrl+s")
	if gs, _ := st.Suggestions("bom"); len(gs) != 1 || gs[0].Signature != "lock wait timeout" {
		t.Fatalf("the task closes the flow's groups: %+v", gs)
	}
}
