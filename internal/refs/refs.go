// Package refs lets the operator reference files in what they write to an
// agent (a message, an approval comment, a ticket): "@path", "@"path with
// spaces"" or a Windows path dragged from Explorer ("@C:\Users\…"). The files
// are copied next to the task, where the agent can always read them (the
// original may sit on a Windows drive, have spaces, or be deleted later), and
// the text gets a list of the copies.
package refs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Limits of one attachment set.
const (
	MaxFiles    = 20
	MaxFileSize = 100 << 20
)

// Dir is where the referenced files of a task are copied.
func Dir(home, taskID string) string { return filepath.Join(home, "refs", taskID) }

// tokenRe finds "@…" at the start of the text or after a space or an opening
// bracket, so e-mail addresses (a@b.c) are not references.
var tokenRe = regexp.MustCompile(`(^|[\s(\[{,;])@(?:"([^"]+)"|'([^']+)'|(\S+))`)

// Token is one reference found in a text.
type Token struct {
	Raw  string // as written, e.g. @"C:\a b\x.pdf"
	Path string // resolved local path
}

// Parse returns the references of text. A bare "@word" without a path
// separator that does not name an existing file is a mention, not a
// reference, and is skipped.
func Parse(text string) []Token {
	var out []Token
	for _, m := range tokenRe.FindAllStringSubmatchIndex(text, -1) {
		raw := text[m[0]:m[1]]
		raw = strings.TrimLeft(raw, " \t\n\r([{,;")
		var p string
		quoted := false
		switch {
		case m[4] >= 0:
			p, quoted = text[m[4]:m[5]], true
		case m[6] >= 0:
			p, quoted = text[m[6]:m[7]], true
		default:
			p = text[m[8]:m[9]]
		}
		path := Resolve(p)
		if !quoted && !exists(path) {
			// "see @spec.pdf." — trailing punctuation is not part of the name.
			if t := strings.TrimRight(p, ".,;:!?)]}"); t != p && exists(Resolve(t)) {
				raw = raw[:len(raw)-(len(p)-len(t))]
				path = Resolve(t)
			}
		}
		if !quoted && !exists(path) && !strings.ContainsAny(p, `/\`) && !strings.HasPrefix(p, "~") {
			continue // a mention like @team
		}
		out = append(out, Token{Raw: raw, Path: path})
	}
	return out
}

var winPath = regexp.MustCompile(`^([A-Za-z]):[\\/](.*)$`)

// Resolve turns what was typed into a local path: ~ expanded, a Windows path
// mapped to its WSL mount (C:\x → /mnt/c/x), a file:// URL unwrapped.
func Resolve(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "file://")
	if m := winPath.FindStringSubmatch(p); m != nil {
		return "/mnt/" + strings.ToLower(m[1]) + "/" + strings.ReplaceAll(m[2], `\`, "/")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Token formats a path as a reference to insert in a text.
func Format(path string) string {
	if strings.ContainsAny(path, " \t\"") {
		return `@"` + path + `"`
	}
	return "@" + path
}

// Attached is one copied file.
type Attached struct {
	Original string
	Copy     string
	Size     int64
}

// Attach copies the files referenced in text into dir and returns the text
// with a list of the copies appended (the references stay as written). A
// reference that does not exist, is a directory or is too large is an error:
// the operator fixes it before anything is sent.
func Attach(text, dir string) (string, []Attached, error) {
	toks := Parse(text)
	if len(toks) == 0 {
		return text, nil, nil
	}
	if len(toks) > MaxFiles {
		return "", nil, fmt.Errorf("tối đa %d tệp mỗi lần", MaxFiles)
	}
	var problems []string
	for _, t := range toks {
		fi, err := os.Stat(t.Path)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("không tìm thấy %s", t.Raw))
		case fi.IsDir():
			problems = append(problems, fmt.Sprintf("%s là thư mục (chỉ đính kèm tệp)", t.Raw))
		case fi.Size() > MaxFileSize:
			problems = append(problems, fmt.Sprintf("%s quá lớn (%d MB, tối đa %d MB)", t.Raw, fi.Size()>>20, MaxFileSize>>20))
		}
	}
	if len(problems) > 0 {
		return "", nil, errors.New(strings.Join(problems, "; "))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	var files []Attached
	done := map[string]string{} // original → copy, for a file referenced twice
	for _, t := range toks {
		if _, ok := done[t.Path]; ok {
			continue
		}
		dst, err := copyInto(t.Path, dir)
		if err != nil {
			return "", nil, fmt.Errorf("chép %s: %w", t.Raw, err)
		}
		fi, _ := os.Stat(dst)
		done[t.Path] = dst
		files = append(files, Attached{Original: t.Path, Copy: dst, Size: fi.Size()})
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, "\n"))
	b.WriteString("\n\nTệp đính kèm (đã chép, đọc bằng Read):\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s (%s; gốc: %s)\n", f.Copy, size(f.Size), f.Original)
	}
	return b.String(), files, nil
}

// copyInto copies src into dir under its base name; a different file with
// the same name gets a -2, -3… suffix, the same file is reused.
func copyInto(src, dir string) (string, error) {
	base := filepath.Base(src)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	sfi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		dst := filepath.Join(dir, name)
		dfi, err := os.Stat(dst)
		if errors.Is(err, os.ErrNotExist) {
			return dst, copyFile(src, dst)
		}
		if err == nil && dfi.Size() == sfi.Size() && sameContent(src, dst) {
			return dst, nil
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func sameContent(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// DefaultPickDir is where the file picker opens first: the Windows Downloads
// folder (files from Telegram, Chrome…) when running in WSL, else home.
func DefaultPickDir() string {
	if m, _ := filepath.Glob("/mnt/c/Users/*/Downloads"); len(m) > 0 {
		for _, d := range m {
			u := filepath.Base(filepath.Dir(d))
			if u != "Public" && u != "Default" && u != "All Users" && u != "Default User" {
				return d
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}
