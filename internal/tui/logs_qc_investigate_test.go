package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/argocd"
	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
)

// qcModel serves two bom QC apps whose logs hold errors and a shared request.
func qcModel(t *testing.T) Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/applications":
			w.Write([]byte(`{"items":[
			 {"metadata":{"name":"bom-qc-bom-api"},"spec":{"destination":{"namespace":"bom-qc"}},"status":{"health":{"status":"Healthy"},"sync":{"status":"Synced","revision":"abcdef1234567"},"resources":[{"group":"apps","kind":"Deployment","name":"bom-api","namespace":"bom-qc"}]}},
			 {"metadata":{"name":"bom-qc-bom-worker"},"spec":{"destination":{"namespace":"bom-qc"}},"status":{"health":{"status":"Healthy"},"sync":{"status":"Synced"},"resources":[{"group":"apps","kind":"Deployment","name":"bom-worker","namespace":"bom-qc"}]}}]}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			app := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/applications/"), "/logs")
			lines := map[string][]string{
				"bom-qc-bom-api": {
					`{"level":"error","message":"lock wait timeout 50","caller":"mo/repo.go:88","request_id":"rq-7"}`,
					`{"level":"error","message":"lock wait timeout 51","caller":"mo/repo.go:88","request_id":"rq-8"}`,
					`{"level":"info","message":"GET /mo rq-7"}`,
				},
				"bom-qc-bom-worker": {
					`{"level":"error","message":"kafka publish failed","request_id":"rq-7"}`,
				},
			}[app]
			f := r.URL.Query().Get("filter")
			for i, l := range lines {
				if f != "" && !strings.Contains(l, f) {
					continue
				}
				w.Write([]byte(`{"result":{"content":` + quoteJSON(l) + `,"timeStamp":"2026-11-11T02:0` + string(rune('0'+i)) + `:00Z","podName":"p"}}` + "\n"))
			}
		}
	}))
	t.Cleanup(srv.Close)
	m, _, _ := newTest(t)
	m.d.Config.ArgoCD = config.ArgoCD{URL: srv.URL, Env: "qc", MaxLogLines: 100}
	m.d.ArgoCD = func(context.Context) (*argocd.Client, error) {
		return &argocd.Client{BaseURL: srv.URL, Token: "t"}, nil
	}
	p := m.d.Config.Projects["bom"]
	p.Repos = map[string]config.Repo{"bom-api": {}, "bom-worker": {}}
	m.d.Config.Projects["bom"] = p
	m, cmd := press(t, m, "3", "e")
	return drainAny(t, m, cmd)
}

func quoteJSON(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func run1(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	tm, _ := m.Update(cmd())
	return tm.(Model)
}

func TestQCErrorGroupsAndInvestigation(t *testing.T) {
	m := qcModel(t)
	if len(m.qcApps()) != 2 {
		t.Fatalf("apps = %v", m.qcApps())
	}
	m, cmd := press(t, m, "g")
	m = run1(t, m, cmd)
	q := m.logs.qc
	if q.view != qcViewGroups || len(q.groups) != 2 || q.groups[0].Count != 2 || q.groups[0].Signature != "lock wait timeout <n>" {
		t.Fatalf("groups = %+v", q.groups)
	}
	if v := m.View(); !strings.Contains(v, "Nhóm lỗi QC") || !strings.Contains(v, "n điều tra · f sửa ngay") {
		t.Fatalf("view:\n%s", v)
	}
	m, _ = press(t, m, "n")
	f := m.modal
	if f == nil || f.value("repo") != "bom-api" || !strings.HasPrefix(f.value("title"), "Điều tra QC bom-qc-bom-api: lock wait timeout") {
		t.Fatalf("investigation form: %+v", f)
	}
	body := f.value("body")
	for _, want := range []string{splunk.QCInvestigationMarker, "Nhóm lỗi trong log QC (Argo CD): app `bom-qc-bom-api`, 2 lần", "mo/repo.go:88", "qc_logs", "qc_apps", "không có trên Splunk"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	m, _ = press(t, m, "esc", "f")
	if m.modal == nil || !strings.HasPrefix(m.modal.value("title"), "Sửa lỗi QC bom-api") || !strings.Contains(m.modal.value("body"), "qc_logs app=bom-qc-bom-api") {
		t.Fatalf("fix form: %+v", m.modal)
	}
}

// F follows one request across every app, sorted by time; n investigates it.
func TestQCSearchAcrossApps(t *testing.T) {
	m := qcModel(t)
	m, _ = press(t, m, "/")          // F still works as an alias
	m, _ = press(t, m, "tab", "tab") // service: all apps · level: any → text
	m = typeText(m, "rq-7")
	m, cmd := press(t, m, "ctrl+s")
	if m.modal != nil {
		t.Fatalf("form: %s", m.modal.err)
	}
	// The form batches "searching" with the fetch; run them in order.
	msgs := cmd().(tea.BatchMsg)
	for _, c := range msgs {
		tm, _ := m.Update(c())
		m = tm.(Model)
	}
	q := m.logs.qc
	if q.view != qcViewSearch || len(q.entries) != 3 {
		t.Fatalf("search = %+v", q.entries)
	}
	apps := map[string]bool{}
	for _, e := range q.entries {
		apps[e.App] = true
	}
	if !apps["bom-qc-bom-api"] || !apps["bom-qc-bom-worker"] {
		t.Fatal("the request is found in both apps")
	}
	m, _ = press(t, m, "n")
	if m.modal == nil || !strings.Contains(m.modal.value("body"), `filter="rq-7"`) || !strings.Contains(m.modal.value("body"), "kafka publish failed") {
		t.Fatalf("investigation from search: %+v", m.modal)
	}
}
