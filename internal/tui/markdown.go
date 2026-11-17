package tui

import (
	"hash/fnv"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
)

// Agents write Markdown (headings, lists, **bold**, `code`, fences, tables).
// mdWrap renders it for the terminal instead of showing the raw syntax: the
// agent's replies, questions, completion and last message.

var md struct {
	sync.Mutex
	renderers map[int]*glamour.TermRenderer
	cache     map[uint64]string
}

// mdMaxCache bounds the rendered-text cache (the conversation is re-rendered
// on every refresh; its texts rarely change).
const mdMaxCache = 2000

// mdStyle is glamour's dark style without the document margin (the panel
// has its own) and without the "##" in front of headings.
func mdStyle() glamour.TermRendererOption {
	st := styles.DarkStyleConfig
	zero := uint(0)
	st.Document.Margin = &zero
	st.Document.BlockPrefix, st.Document.BlockSuffix = "", ""
	st.Code.Prefix, st.Code.Suffix = "", "" // not counted by the word wrap
	for _, h := range []*string{&st.H2.Prefix, &st.H3.Prefix, &st.H4.Prefix, &st.H5.Prefix, &st.H6.Prefix} {
		*h = ""
	}
	// The palette's dark values (palette.go): headings bright instead of
	// blue, inline code on a surface step instead of red on red, links muted.
	tone := func(c string) *string { return &c }
	dark := func(token string) *string { return tone(palette[token].Dark) }
	st.Document.Color = dark(cText)
	st.Heading.Color = dark(cBright)
	st.H1.Color, st.H1.BackgroundColor = dark(cBright), nil
	st.Code.Color, st.Code.BackgroundColor = dark(cText), dark(cSurface)
	st.Link.Color, st.LinkText.Color = dark(cSub), dark(cBright)
	st.HorizontalRule.Color = dark(cRule)
	return glamour.WithStyles(st)
}

func mdRenderer(w int) *glamour.TermRenderer {
	if r := md.renderers[w]; r != nil {
		return r
	}
	// One column of slack: glamour lets a word ending in "-" overflow by one.
	r, err := glamour.NewTermRenderer(mdStyle(), glamour.WithWordWrap(w-1), glamour.WithPreservedNewLines())
	if err != nil {
		return nil
	}
	if md.renderers == nil {
		md.renderers = map[int]*glamour.TermRenderer{}
	}
	md.renderers[w] = r
	return r
}

// mdWrap renders Markdown s to fit w columns, with clickable, shortened URLs
// as wrap makes them.
func mdWrap(s string, w int) string {
	w = max(20, w)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	h := fnv.New64a()
	h.Write([]byte(s))
	h.Write([]byte{byte(w), byte(w >> 8)})
	key := h.Sum64()

	md.Lock()
	defer md.Unlock()
	if out, ok := md.cache[key]; ok {
		return out
	}
	src, links := mdLinks(s, w-4)
	src = mdHyphens(src)
	// glamour's own word wrap misjudges lines with inline code and nested
	// list items, leaving a word alone on a line: render unwrapped and wrap
	// here, keeping each line's indent and bullet. Tables need glamour's
	// width to lay their columns out.
	wide := !mdTableRe.MatchString(src)
	r := mdRenderer(map[bool]int{true: mdUnwrapped, false: w}[wide])
	if r == nil {
		return wrap(s, w)
	}
	out, err := r.Render(src)
	if err != nil {
		return wrap(s, w)
	}
	out = trimRendered(out)
	if wide {
		out = mdRewrap(out, w)
	}
	out = linkify(strings.NewReplacer(nbHyphen, "-", nbSpace, " ").Replace(out), links)
	if md.cache == nil || len(md.cache) >= mdMaxCache {
		md.cache = map[uint64]string{}
	}
	md.cache[key] = out
	return out
}

// mdUnwrapped is the width glamour renders at when mdWrap wraps itself.
const mdUnwrapped = 1000

var (
	mdTableRe  = regexp.MustCompile(`(?m)^\s*\|.*\|\s*$`)
	mdBulletRe = regexp.MustCompile(`^ *(?:[•*-] |\d+\. |│ )?`)
)

// mdRewrap wraps each rendered line wider than w at word boundaries; the
// continuation lines are indented under the text, past the line's bullet.
func mdRewrap(s string, w int) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if ansi.StringWidth(line) <= w {
			out = append(out, line)
			continue
		}
		in := min(ansi.StringWidth(mdBulletRe.FindString(ansi.Strip(line))), w/2)
		head, rest := ansi.Truncate(line, in, ""), ansi.TruncateLeft(line, in, "")
		for i, l := range strings.Split(ansi.Wrap(rest, w-in, ""), "\n") {
			if i == 0 {
				out = append(out, head+l)
			} else {
				out = append(out, strings.Repeat(" ", in)+l)
			}
		}
	}
	return strings.Join(out, "\n")
}

// mdLinkRe matches a Markdown link to a URL, or a bare URL.
var mdLinkRe = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)|https?://[^\s<>"'` + "`" + `]+`)

// mdLinks rewrites the links of s outside code into plain labels: "[text](url)"
// shows text, a bare URL its shortened form without the scheme (glamour
// would otherwise print every URL after its text, in full). The returned
// links make the labels clickable once rendered.
func mdLinks(s string, maxW int) (string, []link) {
	var out strings.Builder
	var links []link
	fence := false
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
		}
		if fence {
			out.WriteString(line)
			continue
		}
		last := 0
		for _, m := range mdLinkRe.FindAllStringSubmatchIndex(line, -1) {
			if strings.Count(line[:m[0]], "`")%2 == 1 { // inside `code`
				continue
			}
			out.WriteString(line[last:m[0]])
			last = m[1]
			if m[2] >= 0 { // [text](url)
				text := line[m[2]:m[3]]
				links = append(links, link{label: mdPlain(text), url: line[m[4]:m[5]]})
				out.WriteString(text)
				continue
			}
			u := trimURL(line[m[0]:m[1]])
			last = m[0] + len(u)
			label := shortenURL(u, maxW)
			if _, rest, ok := strings.Cut(label, "://"); ok {
				label = rest
			}
			links = append(links, link{label: label, url: u})
			out.WriteString(mdEscape(unbreakable(label)))
		}
		out.WriteString(line[last:])
	}
	return out.String(), links
}

// mdEscape keeps Markdown from reading punctuation of a URL as syntax.
func mdEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_[]<>#|~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mdPlain is roughly how a link text shows once rendered (emphasis and code
// marks dropped), to find it in the output.
func mdPlain(s string) string {
	return strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
}

// trimRendered drops the blank lines around glamour's output and the
// padding at the end of each line.
func trimRendered(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = trimLineEnd(l)
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// trimLineEnd cuts a rendered line after its last printed non-space rune,
// resetting the style there; "" when nothing is printed.
func trimLineEnd(l string) string {
	end, styled := -1, false
	for i := 0; i < len(l); {
		if l[i] == 0x1b && i+1 < len(l) {
			i, styled = skipEscape(l, i), true
			continue
		}
		r, n := utf8.DecodeRuneInString(l[i:])
		if r != ' ' {
			end = i + n
		}
		i += n
	}
	if end < 0 {
		return ""
	}
	if styled {
		return l[:end] + "\x1b[0m"
	}
	return l[:end]
}

// nbSpace (U+00A0) joins the words of an inline code span.
const nbSpace = "\u00a0"

// mdHyphens makes the hyphens inside words (branch names, flags, ids) and
// the spaces inside `code` unbreakable: glamour's word wrap misjudges the
// width of a line broken there, which its second wrap then cuts, leaving a
// word alone on a line. mdWrap puts the real characters back after rendering.
func mdHyphens(s string) string {
	var out strings.Builder
	fence := false
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
		}
		if fence {
			out.WriteString(line)
			continue
		}
		rs := []rune(line)
		inWord := false // the previous rune is a letter, digit or joined hyphen
		code := false   // inside `code`
		for j, r := range rs {
			switch {
			case r == '`':
				code = !code
			case r == ' ' && code:
				out.WriteString(nbSpace)
				continue
			case r == '-' && inWord && j+1 < len(rs) && !unicode.IsSpace(rs[j+1]):
				out.WriteString(nbHyphen)
				continue
			case unicode.IsLetter(r) || unicode.IsDigit(r):
				inWord = true
			default:
				inWord = false
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}
