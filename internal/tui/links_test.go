package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

var oscRe = regexp.MustCompile("\x1b\\]8;([^;]*);([^\x1b]*)\x1b\\\\")

// pieces returns, per line, the (id, url, linked text) of each hyperlink.
func pieces(s string) [][3]string {
	var out [][3]string
	for _, line := range strings.Split(s, "\n") {
		for {
			loc := oscRe.FindStringSubmatchIndex(line)
			if loc == nil {
				break
			}
			params, url := line[loc[2]:loc[3]], line[loc[4]:loc[5]]
			if url == "" { // stray close
				line = line[loc[1]:]
				continue
			}
			rest := line[loc[1]:]
			end := strings.Index(rest, osc8("", ""))
			if end < 0 {
				out = append(out, [3]string{params, url, "UNCLOSED"})
				break
			}
			out = append(out, [3]string{params, url, ansi.Strip(rest[:end])})
			line = rest[end:]
		}
	}
	return out
}

// linksTo collects the linked text of every hyperlink to url, one per piece.
func linksTo(s, url string) []string {
	var out []string
	for _, p := range pieces(s) {
		if p[1] == url {
			out = append(out, p[2])
		}
	}
	return out
}

func TestWrapNeverBreaksAURL(t *testing.T) {
	url := "https://bitbucket.org/hasaki/bom-api/pull-requests/new?source=tp%2Fhw-HSK-E8HL95JP&dest=release%2Fqc"
	short := "https://work.hasaki.vn/task/14556670"
	src := "PR đã mở: " + url + ". Xem thêm " + short + ")"
	out := wrap(src, 30)

	for i, l := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(l); w > 30 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}
	long := linksTo(out, url)
	if len(long) != 1 {
		t.Fatalf("the long URL must be one piece on one line, got %q\n%q", long, out)
	}
	if want := shortenURL(url, 26); long[0] != want || !strings.HasPrefix(want, "bitbucket.org/…") || !strings.HasSuffix(url, strings.TrimPrefix(want, "bitbucket.org/…")) {
		t.Fatalf("long URL shown as %q, want %q (host + end of path)", long[0], want)
	}
	if got := linksTo(out, short); len(got) != 1 || got[0] != "work.hasaki.vn/…k/14556670" {
		t.Fatalf("second URL (trailing ')' trimmed): %q", got)
	}
	if strings.Contains(ansi.Strip(out), "hasaki/bom-api/pull") {
		t.Fatal("the long URL must not be printed in full")
	}
}

func TestWrapKeepsURLThatFits(t *testing.T) {
	url := "https://a.vn/x/y"
	out := wrap("xem chi tiết tại trang "+url+" nhé", 20)
	if got := linksTo(out, url); len(got) != 1 || got[0] != url {
		t.Fatalf("a URL that fits must be shown whole on one line: %q\n%q", got, out)
	}
}

func TestShortenURL(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"https://a.vn/x", 40, "https://a.vn/x"},
		{"https://example.com/a/b", 16, "example.com/a/b"},
		{"https://bitbucket.org/hasaki/bom/pull-requests/12345", 30, "bitbucket.org/…-requests/12345"},
		{"https://very-long-host-name.example.com/path/to/x", 20, "very-long-host-name…"},
	} {
		got := shortenURL(c.in, c.max)
		if got != c.want {
			t.Errorf("shortenURL(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if n := len([]rune(got)); n > c.max {
			t.Errorf("shortenURL(%q, %d) is %d long", c.in, c.max, n)
		}
	}
}

func TestLinkifyStyledAndPlain(t *testing.T) {
	if got := wrap("không có link", 40); strings.Contains(got, "\x1b]8") {
		t.Fatal("no URL, no hyperlink")
	}
	url := "https://example.com/a/b"
	out := errStyle.Render(wrap("lỗi tại "+url, 20))
	if got := linksTo(out, url); len(got) != 1 || got[0] != "example.com/a/b" {
		t.Fatalf("styled: %q from %q", got, out)
	}
}

func TestTrimURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.vn/x.":              "https://a.vn/x",
		"https://a.vn/x),":             "https://a.vn/x",
		"https://en.wiki/Go_(lang)":    "https://en.wiki/Go_(lang)",
		"https://a.vn/?q=1&r=[2]":      "https://a.vn/?q=1&r=[2]",
		"https://a.vn/path/to/file.go": "https://a.vn/path/to/file.go",
	} {
		if got := trimURL(in); got != want {
			t.Errorf("trimURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The link must survive the viewport, the box and the side-by-side layout.
func TestPRLinkClickableInView(t *testing.T) {
	m, st, _ := newTest(t)
	m.width, m.height = 100, 40
	m.resize()
	url := "https://bitbucket.org/hasaki/bom-frontend-very-long-repository-name/pull-requests/123456"
	tk := &store.Task{ID: "mock-9", Title: "x", Runs: 1}
	tk.Transition(store.StateDone, "")
	st.SaveTask(tk)
	st.SetPullRequest("mock-9", &store.PullRequest{Repo: "bom-frontend", Destination: "release/qc", ID: 123456, URL: url})
	m.reloadTasks()
	got := linksTo(m.View(), url)
	_, tail, _ := strings.Cut(got[0], "…")
	if len(got) != 1 || !strings.HasSuffix(url, tail) {
		t.Fatalf("PR link in the view must be one piece: %q", got)
	}
}
