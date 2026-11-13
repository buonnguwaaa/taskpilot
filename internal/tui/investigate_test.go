package tui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// investigationServer answers the grouping search with one group and any
// other search with one error event.
func investigationServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"sid":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/results"):
			w.Write([]byte(`{"results":[{"service":"bom-api","sig":"lock wait timeout <n>","sample":"lock wait timeout 50","count":"12",` +
				`"first":"1794217095.2","last":"1794296142.2","caller":"mo/repo.go:88","prefix":"UpdateMO","request_id":"rq-1"}]}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			w.Write([]byte(`{"results":[{"_time":"2026-11-09T10:00:00.000+07:00","_raw":"{\"log\":\"{\\\"level\\\":\\\"error\\\",\\\"message\\\":\\\"boom\\\"}\",\"kubernetes\":{\"container_name\":\"bom-api\"}}"}]}`))
		case r.Method == http.MethodGet:
			w.Write([]byte(`{"entry":[{"content":{"isDone":true}}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var isoRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2} → \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)

func TestInvestigateFromErrorGroup(t *testing.T) {
	srv := investigationServer(t)
	m, _ := logsModel(t, srv.URL)
	m.d.Config.Projects["bom"].Repos["bom-api"] = m.d.Config.Projects["bom"].Repos["bom-api"]
	m, _ = press(t, m, "3", "enter") // Production, first entry: the error grouping
	m, cmd := m, tea.Cmd(nil)
	m, cmd = press(t, m, "r")
	if cmd == nil {
		m, cmd = press(t, m, "enter")
	}
	m = drainLogs(t, m, cmd)
	if len(m.logs.groups) != 1 || !m.logs.groupsFocus {
		t.Fatalf("groups = %+v", m.logs.groups)
	}
	if v := m.View(); !strings.Contains(v, "n điều tra · f sửa ngay") {
		t.Fatalf("group keys not shown:\n%s", v)
	}
	m, _ = press(t, m, "n")
	f := m.modal
	if f == nil || f.value("project") != "bom" || f.value("repo") != "bom-api" || !strings.HasPrefix(f.value("title"), "Điều tra prod bom-api: lock wait timeout") {
		t.Fatalf("investigation form: %+v", f)
	}
	body := f.value("body")
	for _, want := range []string{splunk.InvestigationMarker, "12 lần", "mo/repo.go:88", "UpdateMO", "rq-1", "cổng plan"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if !isoRe.MatchString(body) {
		t.Errorf("the window must be absolute:\n%s", body)
	}
	// f still creates a fix task directly.
	m, _ = press(t, m, "esc", "f")
	if m.modal == nil || !strings.HasPrefix(m.modal.value("title"), "Sửa lỗi prod bom-api") {
		t.Fatal("f opens the fix task form")
	}
}

func TestInvestigateFromSymptomAndResults(t *testing.T) {
	srv := investigationServer(t)
	m, _ := logsModel(t, srv.URL)
	m, _ = press(t, m, "3", "j", "enter") // 🔎 Điều tra sự cố…
	if m.modal == nil || !strings.Contains(m.modal.title, "Điều tra sự cố production") {
		t.Fatal("the investigate entry opens the symptom form")
	}
	m = typeText(m, "Kho báo không lưu được MO từ 9h")
	m, _ = press(t, m, "tab", "right", "tab") // service: bom-api
	m = typeText(m, "MO-77")
	m, cmd := press(t, m, "ctrl+s")
	if m.modal != nil || cmd == nil {
		t.Fatal("the symptom form closes and opens the task form")
	}
	tm, _ := m.Update(cmd())
	m = tm.(Model)
	if m.modal == nil || m.modal.value("title") != "Điều tra prod bom-api: Kho báo không lưu được MO từ 9h" ||
		!strings.Contains(m.modal.value("body"), "MO-77") || !isoRe.MatchString(m.modal.value("body")) {
		t.Fatalf("task form: %q\n%s", m.modal.value("title"), m.modal.value("body"))
	}
	m, _ = press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("submit: %s", m.modal.err)
	}

	// From a search result: n takes its SPL and log lines as evidence.
	m, _ = press(t, m, "/")
	m, cmd = press(t, m, "ctrl+s") // builder with defaults: level=error
	m = drainLogs(t, m, cmd)
	m, _ = press(t, m, "n")
	if m.modal == nil || !strings.Contains(m.modal.value("body"), "level=error") || !strings.Contains(m.modal.value("body"), "boom") {
		t.Fatalf("investigation from results: %+v", m.modal)
	}
}
