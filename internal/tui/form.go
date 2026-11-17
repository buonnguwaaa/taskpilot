package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/refs"
)

type fieldKind int

const (
	fieldSelect fieldKind = iota
	fieldText
	fieldArea
	fieldMulti // several options, Space toggles the highlighted one
)

type option struct{ Label, Value string }

// field is one input of a form.
type field struct {
	key      string
	label    string
	kind     fieldKind
	required bool
	// options returns the choices of a select; it can depend on other fields.
	options func(f *form) []option
	sel     int
	checked map[int]bool // fieldMulti
	input   textinput.Model
	area    textarea.Model
}

// form is a modal dialog: Tab/Shift+Tab move between fields, ←/→ (or ↑/↓)
// change a select, Ctrl+S (or Enter on the last single-line field) submits,
// Esc cancels.
type form struct {
	title    string
	hint     string
	fields   []*field
	focus    int
	err      string
	onSubmit func(v map[string]string) (tea.Cmd, error)
	width    int
	// picker is open (Ctrl+O) to insert a file reference in the focused field.
	picker *picker
}

func newForm(title string, onSubmit func(map[string]string) (tea.Cmd, error)) *form {
	return &form{title: title, onSubmit: onSubmit, width: 80}
}

func (f *form) addSelect(key, label string, opts func(*form) []option, initial string) *form {
	fl := &field{key: key, label: label, kind: fieldSelect, options: opts}
	for i, o := range opts(f) {
		if o.Value == initial {
			fl.sel = i
		}
	}
	f.fields = append(f.fields, fl)
	return f
}

// addMulti adds a field where several options can be ticked (Space).
func (f *form) addMulti(key, label string, opts func(*form) []option, required bool) *form {
	f.fields = append(f.fields, &field{key: key, label: label, kind: fieldMulti, options: opts, required: required, checked: map[int]bool{}})
	return f
}

func (f *form) addText(key, label, initial string, required bool) *form {
	ti := textinput.New()
	ti.CharLimit = 500
	ti.SetValue(initial)
	f.fields = append(f.fields, &field{key: key, label: label, kind: fieldText, required: required, input: ti})
	return f
}

func (f *form) addArea(key, label, initial string, required bool) *form {
	ta := textarea.New()
	ta.CharLimit = 20000
	ta.MaxHeight = 0 // no line limit (the default keeps 99 lines); long pastes go to a file (paste.go)
	ta.ShowLineNumbers = false
	ta.SetHeight(6)
	ta.SetValue(initial)
	f.fields = append(f.fields, &field{key: key, label: label, kind: fieldArea, required: required, area: ta})
	return f
}

// start focuses the first field.
func (f *form) start() tea.Cmd { return f.setFocus(0) }

func (f *form) setFocus(i int) tea.Cmd {
	if len(f.fields) == 0 {
		return nil
	}
	i = (i + len(f.fields)) % len(f.fields)
	for _, fl := range f.fields {
		fl.input.Blur()
		fl.area.Blur()
	}
	f.focus = i
	switch fl := f.fields[i]; fl.kind {
	case fieldText:
		return fl.input.Focus()
	case fieldArea:
		return fl.area.Focus()
	}
	return nil
}

func (f *form) value(key string) string {
	for _, fl := range f.fields {
		if fl.key != key {
			continue
		}
		switch fl.kind {
		case fieldSelect:
			opts := fl.options(f)
			if len(opts) == 0 {
				return ""
			}
			if fl.sel >= len(opts) {
				fl.sel = 0
			}
			return opts[fl.sel].Value
		case fieldMulti:
			var vals []string
			for i, o := range fl.options(f) {
				if fl.checked[i] {
					vals = append(vals, o.Value)
				}
			}
			return strings.Join(vals, "\x1f") // unit separator: values may contain commas
		case fieldText:
			return strings.TrimSpace(fl.input.Value())
		case fieldArea:
			return strings.TrimSpace(fl.area.Value())
		}
	}
	return ""
}

func (f *form) values() map[string]string {
	v := map[string]string{}
	for _, fl := range f.fields {
		v[fl.key] = f.value(fl.key)
	}
	return v
}

func (f *form) submit() (tea.Cmd, bool) {
	for i, fl := range f.fields {
		if fl.required && f.value(fl.key) == "" {
			f.err = fl.label + " không được để trống"
			return f.setFocus(i), false
		}
	}
	cmd, err := f.onSubmit(f.values())
	if err != nil {
		f.err = err.Error()
		return nil, false
	}
	return cmd, true
}

// update handles a key; done reports that the form closed (submitted or cancelled).
func (f *form) update(msg tea.KeyMsg) (cmd tea.Cmd, done bool) {
	fl := f.fields[f.focus]
	if f.picker != nil {
		path, closed := f.picker.update(msg)
		if path != "" {
			f.insert(fl, refs.Format(path))
		}
		if closed {
			f.picker = nil
		}
		return nil, false
	}
	switch msg.String() {
	case "ctrl+o":
		if fl.kind == fieldText || fl.kind == fieldArea {
			f.picker = newPicker()
		}
		return nil, false
	case "esc":
		return nil, true
	case "ctrl+s":
		return f.submit()
	case "tab":
		return f.setFocus(f.focus + 1), false
	case "shift+tab":
		return f.setFocus(f.focus - 1), false
	case "enter":
		if fl.kind == fieldArea {
			break // newline
		}
		if f.focus == len(f.fields)-1 {
			return f.submit()
		}
		return f.setFocus(f.focus + 1), false
	}
	switch fl.kind {
	case fieldSelect:
		n := len(fl.options(f))
		if n > 0 {
			switch msg.String() {
			case "left", "up", "h", "k":
				fl.sel = (fl.sel - 1 + n) % n
			case "right", "down", "l", "j", " ":
				fl.sel = (fl.sel + 1) % n
			}
		}
	case fieldMulti:
		n := len(fl.options(f))
		if n > 0 {
			switch msg.String() {
			case "left", "up", "h", "k":
				fl.sel = (fl.sel - 1 + n) % n
			case "right", "down", "l", "j":
				fl.sel = (fl.sel + 1) % n
			case " ", "x":
				fl.checked[fl.sel] = !fl.checked[fl.sel]
			}
		}
	case fieldText:
		fl.input, cmd = fl.input.Update(msg)
	case fieldArea:
		fl.area, cmd = fl.area.Update(msg)
	}
	return cmd, false
}

// multiValues splits a fieldMulti value.
func multiValues(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, "\x1f")
}

// insert adds a file reference at the cursor of a text field.
func (f *form) insert(fl *field, token string) {
	switch fl.kind {
	case fieldArea:
		v := fl.area.Value()
		if v != "" && !strings.HasSuffix(v, " ") && !strings.HasSuffix(v, "\n") {
			token = " " + token
		}
		fl.area.InsertString(token + " ")
	case fieldText:
		v := fl.input.Value()
		if v != "" && !strings.HasSuffix(v, " ") {
			v += " "
		}
		fl.input.SetValue(v + token + " ")
		fl.input.CursorEnd()
	}
}

func (f *form) view(width int) string {
	w := min(max(50, width-10), 110)
	if f.picker != nil {
		return modalStyle.Width(w).Render(f.picker.view(w-6, 18))
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.title) + "\n")
	if f.hint != "" {
		b.WriteString(dimStyle.Render(f.hint) + "\n")
	}
	b.WriteString("\n")
	for i, fl := range f.fields {
		label := fl.label
		if fl.required {
			label += " *"
		}
		marker := "  "
		ls := dimStyle
		if i == f.focus {
			marker, ls = "▸ ", accentStyle
		}
		b.WriteString(marker + ls.Render(label) + "\n")
		switch fl.kind {
		case fieldSelect:
			b.WriteString(wrapOptions(fl.options(f), fl.sel, w-8, "    ") + "\n")
		case fieldMulti:
			opts := fl.options(f)
			marked := make([]option, len(opts))
			for j, o := range opts {
				box := "[ ] "
				if fl.checked[j] {
					box = "[x] "
				}
				marked[j] = option{Label: box + o.Label, Value: o.Value}
			}
			b.WriteString(wrapOptions(marked, fl.sel, w-8, "    ") + "\n")
		case fieldText:
			fl.input.Width = w - 6
			b.WriteString("  " + fl.input.View() + "\n")
		case fieldArea:
			fl.area.SetWidth(w - 6)
			b.WriteString(indent(fl.area.View(), "  ") + "\n")
		}
		b.WriteString("\n")
	}
	if f.err != "" {
		b.WriteString(errStyle.Render("✗ "+f.err) + "\n")
	}
	help := "Tab/Shift+Tab: chuyển ô · ←/→: chọn · Ctrl+S: gửi · Esc: huỷ"
	if f.fields[f.focus].kind == fieldMulti {
		help += " · Space: đánh dấu"
	}
	if fl := f.fields[f.focus]; fl.kind == fieldText || fl.kind == fieldArea {
		help += " · Ctrl+O: đính kèm tệp (hoặc gõ @đường/dẫn) · Ctrl+V/Alt+V: dán ảnh/tệp từ clipboard"
	}
	b.WriteString(dimStyle.Render(help))
	return modalStyle.Width(w).Render(b.String())
}

// wrapOptions lays out select options on as many lines as needed, never
// splitting an option, each line indented by pad.
func wrapOptions(opts []option, sel, width int, pad string) string {
	var lines []string
	cur, curW := "", 0
	for j, o := range opts {
		label := " " + o.Label + " "
		styled := dimStyle.Render(label)
		if j == sel {
			styled = selStyle.Render(label)
		}
		w := lipgloss.Width(label)
		if curW > 0 && curW+1+w > width {
			lines = append(lines, pad+cur)
			cur, curW = "", 0
		}
		if curW > 0 {
			cur += " "
			curW++
		}
		cur += styled
		curW += w
	}
	if curW > 0 || len(lines) == 0 {
		lines = append(lines, pad+cur)
	}
	return strings.Join(lines, "\n")
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// confirm is a yes/no modal.
type confirm struct {
	text  string
	onYes func() tea.Cmd
}

func (c *confirm) view(width int) string {
	w := min(max(40, width-20), 90)
	return modalStyle.Width(w).Render(fmt.Sprintf("%s\n\n%s", c.text, dimStyle.Render("y: đồng ý · n/Esc: huỷ")))
}
