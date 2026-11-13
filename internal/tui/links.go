package tui

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// urlRe matches the URLs worth making clickable. Trailing punctuation that
// usually ends a sentence is trimmed by trimURL.
var urlRe = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

func trimURL(u string) string {
	for {
		t := strings.TrimRight(u, ".,;:!?")
		// A closing bracket belongs to the URL only if it opens one too.
		for _, p := range [][2]string{{"(", ")"}, {"[", "]"}} {
			if strings.HasSuffix(t, p[1]) && strings.Count(t, p[0]) < strings.Count(t, p[1]) {
				t = t[:len(t)-1]
			}
		}
		if t == u {
			return t
		}
		u = t
	}
}

// findURLs lists the URLs in s, in order.
func findURLs(s string) []string {
	var out []string
	for _, u := range urlRe.FindAllString(s, -1) {
		if u = trimURL(u); len(u) > len("https://") {
			out = append(out, u)
		}
	}
	return out
}

// osc8 opens (url != "") or closes a terminal hyperlink. Pieces of one URL
// share the id, so terminals treat a wrapped link as a single link.
func osc8(id, url string) string {
	if url == "" {
		return "\x1b]8;;\x1b\\"
	}
	return "\x1b]8;id=" + id + ";" + url + "\x1b\\"
}

// visible is a printed rune of rendered text and where its bytes are.
type visible struct {
	r          rune
	start, end int // byte offsets in the rendered string
	line       int
}

// scanVisible returns the printed runes of s, skipping ANSI escape sequences
// (CSI and OSC/DCS-style strings), newlines and each line's trailing spaces
// (the padding lipgloss adds), so a URL broken over lines reads contiguously.
func scanVisible(s string) []visible {
	var out []visible
	line, lineStart := 0, 0
	flush := func() { // drop trailing spaces of the current line
		for len(out) > lineStart && out[len(out)-1].line == line && out[len(out)-1].r == ' ' {
			out = out[:len(out)-1]
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			i = skipEscape(s, i)
			continue
		}
		if c == '\n' {
			flush()
			line++
			lineStart = len(out)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		out = append(out, visible{r: r, start: i, end: i + n, line: line})
		i += n
	}
	flush()
	return out
}

// skipEscape returns the offset just past the escape sequence starting at i.
func skipEscape(s string, i int) int {
	switch s[i+1] {
	case '[': // CSI: parameters then a final byte 0x40–0x7E
		j := i + 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		return min(j+1, len(s))
	case ']', 'P', '_', '^': // string: ends with BEL or ST (ESC \)
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	return min(i+2, len(s))
}

// link is a URL and the text that shows it.
type link struct{ label, url string }

// shortenURLs replaces every URL of s wider than max columns by a shorter
// label, so no URL is ever broken over lines: terminals handle a hyperlink
// split across lines badly (only one piece opens, or the wrong URL).
func shortenURLs(s string, max int) (string, []link) {
	urls := findURLs(s)
	if len(urls) == 0 {
		return s, nil
	}
	var b strings.Builder
	links := make([]link, 0, len(urls))
	rest := s
	for _, u := range urls {
		i := strings.Index(rest, u)
		if i < 0 {
			continue
		}
		l := link{label: shortenURL(u, max), url: u}
		b.WriteString(rest[:i])
		b.WriteString(unbreakable(l.label))
		rest = rest[i+len(u):]
		links = append(links, l)
	}
	b.WriteString(rest)
	return b.String(), links
}

// nbHyphen is U+2011, a hyphen word wrapping does not break at. Labels
// are wrapped with it so a link never splits over lines (terminals open
// only the piece clicked), then it is put back (restoreLabels).
const nbHyphen = "\u2011"

func unbreakable(label string) string { return strings.ReplaceAll(label, "-", nbHyphen) }

// restoreLabels puts the real hyphens back in the wrapped labels.
func restoreLabels(s string, links []link) string {
	for _, l := range links {
		if nb := unbreakable(l.label); nb != l.label {
			s = strings.ReplaceAll(s, nb, l.label)
		}
	}
	return s
}

// shortenURL fits u into max columns: drop the scheme, then keep the host and
// the end of the path ("bitbucket.org/…/pull-requests/12"), the parts that
// tell links apart.
func shortenURL(u string, max int) string {
	if utf8.RuneCountInString(u) <= max {
		return u
	}
	_, rest, _ := strings.Cut(u, "://")
	r := []rune(rest)
	if len(r) <= max {
		return rest
	}
	host, _, _ := strings.Cut(rest, "/")
	if budget := max - utf8.RuneCountInString(host) - 2; budget >= 8 {
		return host + "/…" + string(r[len(r)-budget:])
	}
	return string(r[:max-1]) + "…"
}

// linkify makes the labels of links clickable in rendered, the wrapped (and
// possibly styled) text that shows them. Should a label still be broken over
// lines, every piece gets its own OSC 8 open/close pair sharing one id.
// Width is unchanged.
func linkify(rendered string, links []link) string {
	if len(links) == 0 {
		return rendered
	}
	vis := scanVisible(rendered)
	flat := make([]rune, len(vis))
	for i, v := range vis {
		flat[i] = v.r
	}
	type ins struct {
		at   int
		text string
		ord  int // at the same offset: closes before opens
	}
	var inserts []ins
	cursor := 0
	for _, l := range links {
		lr := []rune(l.label)
		at := indexRunes(flat, lr, cursor)
		if at < 0 {
			continue
		}
		cursor = at + len(lr)
		h := fnv.New32a()
		h.Write([]byte(l.url))
		id := fmt.Sprintf("tp%x-%d", h.Sum32(), at)
		for i := at; i < at+len(lr); {
			j := i
			for j+1 < at+len(lr) && vis[j+1].line == vis[i].line {
				j++
			}
			inserts = append(inserts, ins{vis[i].start, osc8(id, l.url), 1}, ins{vis[j].end, osc8("", ""), 0})
			i = j + 1
		}
	}
	sort.SliceStable(inserts, func(a, b int) bool {
		if inserts[a].at != inserts[b].at {
			return inserts[a].at > inserts[b].at
		}
		return inserts[a].ord > inserts[b].ord // applied back to front: opens first, so closes end up before
	})
	out := rendered
	for _, in := range inserts {
		out = out[:in.at] + in.text + out[in.at:]
	}
	return out
}

func indexRunes(hay, needle []rune, from int) int {
outer:
	for i := from; i+len(needle) <= len(hay); i++ {
		for j, r := range needle {
			if hay[i+j] != r {
				continue outer
			}
		}
		return i
	}
	return -1
}
