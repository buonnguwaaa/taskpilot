package intake

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Claude: config.Claude{Bin: "claude", Model: "sonnet"},
		Projects: map[string]config.Project{
			"bom": {Name: "BOM", Repos: map[string]config.Repo{
				"bom-api":    {Description: "REST API của BOM,\n  MO và export"},
				"bom-worker": {},
			}},
			"plm": {Name: "PLM", Repos: map[string]config.Repo{"hsk-pdp-backend": {}}},
		},
	}
}

func fake(out string, gotArgs *[]string, gotStdin *string) func(context.Context, []string, string) ([]byte, error) {
	return func(_ context.Context, args []string, stdin string) ([]byte, error) {
		if gotArgs != nil {
			*gotArgs = args
		}
		if gotStdin != nil {
			*gotStdin = stdin
		}
		return []byte(out), nil
	}
}

func TestDraftStructuredOutput(t *testing.T) {
	var args []string
	var stdin string
	d := &Drafter{Config: testConfig(), Run: fake(`{"type":"result","is_error":false,"result":"",
		"structured_output":{"project":"bom","repo":"bom-api","title":"Sửa export MO","body":"Export MO bị timeout.\n\nAcceptance criteria:\n- export 10k dòng < 30s","note":""}}`, &args, &stdin)}
	dr, err := d.Draft(context.Background(), "  export MO ở bom-api bị timeout khi >10k dòng  ")
	if err != nil {
		t.Fatal(err)
	}
	if dr.Project != "bom" || dr.Repo != "bom-api" || dr.Title != "Sửa export MO" || dr.Note != "" {
		t.Fatalf("draft = %+v", dr)
	}
	for _, want := range []string{"--json-schema", "--no-session-persistence"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %v", want, args)
		}
	}
	if i := slices.Index(args, "--tools"); i < 0 || args[i+1] != "" {
		t.Errorf("tools must be disabled: %v", args)
	}
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "sonnet" {
		t.Errorf("model not passed: %v", args)
	}
	for _, want := range []string{`project "bom" (BOM)`, `repo "bom-api": REST API của BOM, MO và export`, `repo "hsk-pdp-backend"`,
		"<<<\nexport MO ở bom-api bị timeout khi >10k dòng\n>>>"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("prompt lacks %q:\n%s", want, stdin)
		}
	}
	tk := dr.Ticket("export MO ở bom-api bị timeout khi >10k dòng")
	if tk.Project != "bom" || tk.Repo != "bom-api" || !strings.HasPrefix(tk.Body, "Export MO bị timeout.") ||
		!strings.HasSuffix(tk.Body, "Operator's original request:\nexport MO ở bom-api bị timeout khi >10k dòng") {
		t.Fatalf("ticket = %+v", tk)
	}
}

func TestDraftResultTextFallback(t *testing.T) {
	d := &Drafter{Config: testConfig(), Run: fake(`{"is_error":false,"result":"`+"```json\\n"+`{\"project\":\"plm\",\"repo\":\"\",\"title\":\"T\",\"body\":\"B\",\"note\":\"\"}`+"\\n```"+`"}`, nil, nil)}
	dr, err := d.Draft(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if dr.Project != "plm" || dr.Title != "T" {
		t.Fatalf("draft = %+v", dr)
	}
}

func TestDraftValidatesAgainstConfig(t *testing.T) {
	cases := []struct {
		name, out, project, repo, note string
	}{
		{"unknown project", `{"project":"erp","repo":"erp-api","title":"T","body":"B","note":""}`, "", "", `project "erp" không có trong config`},
		{"repo of another project", `{"project":"plm","repo":"bom-api","title":"T","body":"B","note":"đoán"}`, "plm", "", `đoán · repo "bom-api" không thuộc plm`},
		{"repo without project", `{"project":"","repo":"bom-api","title":"T","body":"B","note":""}`, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Drafter{Config: testConfig(), Run: fake(`{"structured_output":`+c.out+`}`, nil, nil)}
			dr, err := d.Draft(context.Background(), "x")
			if err != nil {
				t.Fatal(err)
			}
			if dr.Project != c.project || dr.Repo != c.repo || dr.Note != c.note {
				t.Fatalf("draft = %+v", dr)
			}
		})
	}
}

func TestDraftErrors(t *testing.T) {
	for name, out := range map[string]string{
		"is_error":  `{"is_error":true,"subtype":"error_max_turns","result":"boom"}`,
		"not json":  `oops`,
		"empty":     `{"structured_output":{"project":"","repo":"","title":"","body":"","note":""}}`,
		"bad draft": `{"result":"I cannot help"}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := &Drafter{Config: testConfig(), Run: fake(out, nil, nil)}
			if _, err := d.Draft(context.Background(), "x"); err == nil {
				t.Fatal("want error")
			}
		})
	}
	d := &Drafter{Config: testConfig(), Run: fake("{}", nil, nil)}
	if _, err := d.Draft(context.Background(), "   "); err == nil {
		t.Fatal("empty prompt should fail")
	}
}
