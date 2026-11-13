package worklog

import (
	"context"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func TestPromptCarriesTheMaterial(t *testing.T) {
	p := Prompt(Input{Language: "Vietnamese", Project: "BOM (Manufactory)", Parent: "HSK-SRC — [Planning MO] Chặn gen WO", Subtask: true,
		Title: "Chặn gen WO khi SKU inactive", Completion: "Đã thêm guard ở bom-report", PRs: []string{"bom-report → release/qc: fix — https://x/286"},
		Gates:    []string{"plan-2: chặn ở bom-report (duyệt) | operator: query có ảnh hưởng hiệu năng không"},
		Examples: []string{"[BE] Enhance report staff query latency", "[Issue Production] Fix manufactor mismatching"}})
	for _, want := range []string{"SUBTASK under the Hasaki Work task: HSK-SRC", "[BE] Enhance report staff query latency",
		"Đã thêm guard ở bom-report", "https://x/286", "operator: query có ảnh hưởng hiệu năng không", "Operator language: Vietnamese"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestDraft(t *testing.T) {
	var gotArgs []string
	d := &Drafter{Config: &config.Config{Claude: config.Claude{Bin: "claude", Model: "opus", WorklogModel: "haiku"}}, Run: func(_ context.Context, args []string, stdin string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"is_error":false,"structured_output":{"name":" \"[BE] Chặn gen WO khi BOM có SKU inactive\" ","result":"Thêm kiểm tra SKU inactive.\nPR: https://x/286"}}`), nil
	}}
	dr, err := d.Draft(context.Background(), Input{Title: "x", Completion: "y"})
	if err != nil || dr.Name != "[BE] Chặn gen WO khi BOM có SKU inactive" || !strings.HasPrefix(dr.Result, "Thêm kiểm tra") {
		t.Fatalf("%+v %v", dr, err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--tools  --no-session-persistence") {
		t.Fatalf("no tools, no session: %q", gotArgs)
	}
	if j := strings.Join(gotArgs, " "); !strings.Contains(j, "--model haiku") || strings.Contains(j, "opus") {
		t.Fatalf("the report is drafted with the light model: %q", gotArgs)
	}
	d.Run = func(context.Context, []string, string) ([]byte, error) {
		return []byte(`{"is_error":false,"result":"Here: {\"name\":\"\",\"result\":\"\"}"}`), nil
	}
	if _, err := d.Draft(context.Background(), Input{Title: "x"}); err == nil {
		t.Fatal("an empty draft is an error")
	}
	if _, err := d.Draft(context.Background(), Input{}); err == nil {
		t.Fatal("nothing to draft")
	}
}
