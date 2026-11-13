package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
	"github.com/buonnguwaaa/taskpilot/internal/graphify"
	"github.com/buonnguwaaa/taskpilot/internal/splunk"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestGraphifyTool(t *testing.T) {
	st, _ := store.New(t.TempDir())
	tk := &store.Task{ID: "mock-g", Project: "bom", Body: "sửa"}
	tk.Transition(store.StateRunning, "")
	st.SaveTask(tk)
	dir := t.TempDir()
	// A fake graphify that prints its arguments.
	bin := filepath.Join(dir, "graphify")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"graphify $*\"\n"), 0o755)
	cfg := &config.Config{Graphify: &config.Graphify{Dir: dir, Bin: bin, Lines: map[string]string{"prod": "release/prod", "qc": "release/qc"}},
		Projects: map[string]config.Project{"bom": {Name: "BOM", Repos: map[string]config.Repo{"bom-worker": {}, "bom-report": {}}}}}
	h := &Handlers{Store: st, Config: cfg, TaskID: "mock-g"}
	ctx := context.Background()

	if _, err := h.Graphify(ctx, GraphifyIn{Action: "affected", Text: "x"}); err == nil || !strings.Contains(err.Error(), "no graphify graph for bom/prod") {
		t.Fatalf("not built yet: %v", err)
	}
	ld := graphify.LineDir(dir, "bom", "prod")
	os.MkdirAll(ld, 0o755)
	os.WriteFile(graphify.GraphPath(dir, "bom", "prod"), []byte(`{"nodes":[],"links":[]}`), 0o644)
	links := []graphify.Link{{Method: "/generate.GenerateService/GenerateWorkOrderV2",
		Consumers: []graphify.Site{{Repo: "bom-worker", File: "usecase/gen.go", Line: 502, Func: "(taskUsecase).genWOViaBomReport"}},
		Providers: []graphify.Site{{Repo: "bom-report", File: "service/handle.go", Line: 9, Func: "GenerateWorkOrderV2"}}}}
	b, _ := json.Marshal(links)
	os.WriteFile(graphify.LinksPath(dir, "bom", "prod"), b, 0o644)

	out, err := h.Graphify(ctx, GraphifyIn{Action: "links", Text: "GenerateWorkOrder"})
	if err != nil || !strings.Contains(out, "[graphify bom/prod]") || !strings.Contains(out, "called by bom-worker usecase/gen.go:502") {
		t.Fatalf("links: %q %v", out, err)
	}
	out, err = h.Graphify(ctx, GraphifyIn{Action: "affected", Text: "bom-report::service_generateworkorderv2"})
	if err != nil || !strings.Contains(out, "affected bom-report::service_generateworkorderv2") || !strings.Contains(out, "--relation calls_grpc") ||
		!strings.Contains(out, "--graph "+graphify.GraphPath(dir, "bom", "prod")) {
		t.Fatalf("affected: %q %v", out, err)
	}
	if _, err := h.Graphify(ctx, GraphifyIn{Action: "links", Line: "task"}); err == nil || !strings.Contains(err.Error(), "no repo open yet") {
		t.Fatalf("task line before open_repo: %v", err)
	}
	if _, err := h.Graphify(ctx, GraphifyIn{Action: "links", Line: "staging"}); err == nil || !strings.Contains(err.Error(), "lines: prod, qc, task") {
		t.Fatalf("unknown line: %v", err)
	}
	// A QC investigation reads the qc line by default.
	tk.Body = splunk.QCInvestigationMarker + "\n..."
	st.SaveTask(tk)
	if _, err := h.Graphify(ctx, GraphifyIn{Action: "links"}); err == nil || !strings.Contains(err.Error(), "bom/qc") {
		t.Fatalf("qc default: %v", err)
	}
	info, _ := h.TaskInfo()
	if !strings.Contains(info, `"graphify"`) || !strings.Contains(info, "graphify tool") {
		t.Fatalf("task_info:\n%s", info)
	}
	// Not configured: a clear refusal.
	h.Config.Graphify = nil
	if _, err := h.Graphify(ctx, GraphifyIn{Action: "links"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatal(err)
	}
}
