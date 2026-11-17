package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestClickOnPRNumberOpensIt(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-pr", Title: "Sort boms", Project: "bom"}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	url := "https://bitbucket.org/x/hsk-pdp-backend/pull-requests/1040"
	st.SetPullRequest("mock-pr", &store.PullRequest{Repo: "hsk-pdp-backend", ID: 1040, URL: url, Destination: "release/qc", Title: "Merge fix/sort into release/qc"})
	m.reloadTasks()
	m.refreshTaskDetail(true)
	var opened []string
	m.d.OpenURL = func(u string) error { opened = append(opened, u); return nil }

	x, y := -1, -1
	for i, l := range strings.Split(m.screen(), "\n") {
		if c := strings.Index(ansi.Strip(l), "#1040"); c >= 0 {
			x, y = ansi.StringWidth(ansi.Strip(l)[:c])+2, i
		}
	}
	if y < 0 {
		t.Fatal("PR line not on screen")
	}
	m = mouse(m, x, y, tea.MouseActionPress)
	m = mouse(m, x, y, tea.MouseActionRelease)
	if len(opened) != 1 || opened[0] != url {
		t.Fatalf("click on #1040 opened %v", opened)
	}
	// Elsewhere on the line: nothing opens.
	m = mouse(m, x+12, y, tea.MouseActionPress)
	m = mouse(m, x+12, y, tea.MouseActionRelease)
	if len(opened) != 1 {
		t.Fatalf("click next to the link opened %v", opened)
	}
}

func TestLinkAt(t *testing.T) {
	line := "ab " + hyperlink("#12", "https://x/12") + " cd đ " + hyperlink("tiếp", "https://y")
	for x, want := range map[int]string{0: "", 3: "https://x/12", 5: "https://x/12", 6: "", 12: "https://y", 15: "https://y", 16: ""} {
		if got := linkAt(line, x); got != want {
			t.Errorf("x=%d: %q, want %q", x, got, want)
		}
	}
}
