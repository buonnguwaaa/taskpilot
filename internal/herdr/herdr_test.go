package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fake(t *testing.T) (*Client, func() string) {
	t.Helper()
	bin, _ := filepath.Abs("testdata/fake-herdr.sh")
	log := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("FAKE_HERDR_LOG", log)
	return &Client{Bin: bin}, func() string { b, _ := os.ReadFile(log); return string(b) }
}

func TestTabSplitRun(t *testing.T) {
	c, args := fake(t)
	ctx := context.Background()
	tab, pane, err := c.CreateTab(ctx, "taskpilot daemon", "/home/u/.taskpilot")
	if err != nil || tab != "w1:t9" || pane != "w1:p42" {
		t.Fatalf("create = %s %s %v", tab, pane, err)
	}
	p, err := c.SplitRight(ctx, "/ws/task")
	if err != nil || p != "w1:p43" {
		t.Fatalf("split = %s %v", p, err)
	}
	cmd := Command("/usr/bin/taskpilot", "takeover", "hw-HSK-1", "-config", "/home/u/my config.yaml")
	if err := c.Run(ctx, p, cmd); err != nil {
		t.Fatal(err)
	}
	got := args()
	for _, want := range []string{
		"tab\ncreate\n--label\ntaskpilot daemon\n--no-focus\n--cwd\n/home/u/.taskpilot\n--",
		"pane\nsplit\n--current\n--direction\nright\n--focus\n--cwd\n/ws/task\n--",
		"pane\nrun\nw1:p43\n/usr/bin/taskpilot takeover hw-HSK-1 -config '/home/u/my config.yaml'\n--",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing:\n%s\n--- got:\n%s", want, got)
		}
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{"a-b_c/d.e:f=g@h": "a-b_c/d.e:f=g@h", "a b": "'a b'", "it's": `'it'"'"'s'`, "": "''", "$(x)": "'$(x)'"}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
