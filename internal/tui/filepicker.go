package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/refs"
)

// lastPickDir is where the picker reopens (per process).
var lastPickDir string

// picker browses the file system to reference a file in a form field.
type picker struct {
	dir     string
	entries []os.DirEntry
	cursor  int
	filter  string // typed letters narrow the list
	err     string
}

func newPicker() *picker {
	dir := lastPickDir
	if dir == "" {
		dir = refs.DefaultPickDir()
	}
	p := &picker{}
	p.open(dir)
	return p
}

func (p *picker) open(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		p.err = err.Error()
		return
	}
	// Directories first, then files; hidden ones last.
	sort.SliceStable(entries, func(i, j int) bool {
		hi, hj := strings.HasPrefix(entries[i].Name(), "."), strings.HasPrefix(entries[j].Name(), ".")
		if hi != hj {
			return hj
		}
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	p.dir, p.entries, p.cursor, p.filter, p.err = dir, entries, 0, "", ""
	lastPickDir = dir
}

// visible are the entries matching the filter.
func (p *picker) visible() []os.DirEntry {
	if p.filter == "" {
		return p.entries
	}
	var out []os.DirEntry
	f := fold(p.filter)
	for _, e := range p.entries {
		if strings.Contains(fold(e.Name()), f) {
			out = append(out, e)
		}
	}
	return out
}

// update handles a key; it returns the chosen file, or done=true when the
// picker closes without a choice.
func (p *picker) update(k tea.KeyMsg) (chosen string, done bool) {
	vis := p.visible()
	switch k.String() {
	case "esc":
		if p.filter != "" {
			p.filter, p.cursor = "", 0
			return "", false
		}
		return "", true
	case "up", "ctrl+p":
		p.cursor = max(0, p.cursor-1)
	case "down", "ctrl+n":
		p.cursor = min(max(0, len(vis)-1), p.cursor+1)
	case "pgup":
		p.cursor = max(0, p.cursor-10)
	case "pgdown":
		p.cursor = min(max(0, len(vis)-1), p.cursor+10)
	case "left", "backspace":
		if k.String() == "backspace" && p.filter != "" {
			r := []rune(p.filter)
			p.filter, p.cursor = string(r[:len(r)-1]), 0
			return "", false
		}
		if parent := filepath.Dir(p.dir); parent != p.dir {
			from := filepath.Base(p.dir)
			p.open(parent)
			for i, e := range p.entries {
				if e.Name() == from {
					p.cursor = i
				}
			}
		}
	case "right", "enter":
		if p.cursor >= len(vis) {
			return "", false
		}
		e := vis[p.cursor]
		path := filepath.Join(p.dir, e.Name())
		if isDir(path, e) {
			p.open(path)
			return "", false
		}
		if k.String() == "enter" {
			return path, true
		}
	case "~":
		if h, err := os.UserHomeDir(); err == nil {
			p.open(h)
		}
	default:
		if r := k.Runes; k.Type == tea.KeyRunes && len(r) > 0 {
			p.filter += string(r)
			p.cursor = 0
		}
	}
	return "", false
}

// isDir follows symlinks (a link to a directory opens it).
func isDir(path string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func (p *picker) view(width, height int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Chọn tệp đính kèm") + "  " + dimStyle.Render(truncate(p.dir, width-22)) + "\n")
	if p.filter != "" {
		b.WriteString(accentStyle.Render("lọc: "+p.filter) + "\n")
	}
	if p.err != "" {
		b.WriteString(errStyle.Render(p.err) + "\n")
	}
	vis := p.visible()
	rows := max(5, height)
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	if len(vis) == 0 {
		b.WriteString(dimStyle.Render("(trống)") + "\n")
	}
	for i := start; i < len(vis) && i-start < rows; i++ {
		e := vis[i]
		name := e.Name()
		if isDir(filepath.Join(p.dir, name), e) {
			name += "/"
		} else if fi, err := e.Info(); err == nil {
			name = fmt.Sprintf("%s  %s", name, dimStyle.Render(humanSize(fi.Size())))
		}
		line := truncate(name, width-4)
		if i == p.cursor {
			line = selStyle.Render(" " + line + " ")
		} else {
			line = " " + line
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(dimStyle.Render("↑↓ chọn · Enter/→ mở thư mục, Enter chọn tệp · ←/Backspace thư mục cha · gõ để lọc · ~ về home · Esc đóng"))
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
