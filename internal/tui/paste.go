package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/desktop"
	"github.com/buonnguwaaa/taskpilot/internal/refs"
)

// Pasting an image or files into a form (Ctrl+V or Alt+V): a terminal
// pastes text only, so a screenshot on the clipboard never reaches the TUI.
// The TUI reads the clipboard itself, saves an image as a PNG, and inserts
// "@file" references like Ctrl+O; the files are copied next to the task on
// submit. When the clipboard holds text only, Ctrl+V pastes the text (the
// terminal hands Ctrl+V over only when it does not paste itself).

type clipPastedMsg struct {
	paths []string
	text  string
	err   error
}

func (m Model) pasteIntoForm() (tea.Model, tea.Cmd) {
	fl := m.modal.fields[m.modal.focus]
	if fl.kind != fieldText && fl.kind != fieldArea {
		m.msg = "dán: chọn ô nhập chữ trước (Tab)"
		return m, nil
	}
	if m.d.PasteFiles == nil || m.d.Store == nil {
		return m, nil
	}
	dir := filepath.Join(m.d.Store.Home, "refs", "_clipboard")
	files, text := m.d.PasteFiles, m.d.PasteText
	m.msg = "đang đọc clipboard…"
	return m, func() tea.Msg {
		ctx := context.Background()
		paths, err := files(ctx, dir)
		if errors.Is(err, desktop.ErrClipboardEmpty) && text != nil {
			s, terr := text(ctx)
			return clipPastedMsg{text: s, err: terr}
		}
		return clipPastedMsg{paths: paths, err: err}
	}
}

func (m Model) onClipPasted(msg clipPastedMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.modal == nil:
		return m, nil
	case msg.err != nil:
		m.msg = "dán từ clipboard: " + msg.err.Error()
		return m, nil
	}
	fl := m.modal.fields[m.modal.focus]
	if len(msg.paths) == 0 {
		m.msg = ""
		return m.insertPaste(msg.text), nil
	}
	for _, p := range msg.paths {
		m.modal.insert(fl, refs.Format(p))
	}
	m.msg = fmt.Sprintf("đã dán %d tệp từ clipboard (agent nhận bản copy khi gửi)", len(msg.paths))
	return m, nil
}

// A long paste (a JSON response, a log) does not fit a form field: the
// field keeps a few thousand characters and 99 lines, and typing in it slows
// down. It is saved to a file instead, with a "@file" reference in the field,
// so the agent gets every character.
const (
	pasteMaxLines = 60
	pasteMaxChars = 4000
)

func longPaste(s string) bool {
	return len(s) > pasteMaxChars || strings.Count(s, "\n") >= pasteMaxLines
}

// pasteLong saves text next to the clipboard images and returns its path.
func (m Model) pasteLong(text string) (string, error) {
	dir := filepath.Join(m.d.Store.Home, "refs", "_clipboard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := ".txt"
	if json.Valid([]byte(strings.TrimSpace(text))) {
		ext = ".json"
	}
	p := filepath.Join(dir, "paste-"+time.Now().Format("20060102-150405.000")+ext)
	return p, os.WriteFile(p, []byte(text), 0o644)
}

// insertPaste puts pasted text in the focused field: as is, or as a file
// reference when it is long.
func (m Model) insertPaste(text string) Model {
	fl := m.modal.fields[m.modal.focus]
	if fl.kind != fieldText && fl.kind != fieldArea {
		return m
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if longPaste(text) && m.d.Store != nil {
		p, err := m.pasteLong(text)
		if err != nil {
			m.msg = "lưu đoạn dán: " + err.Error()
			return m
		}
		m.modal.insert(fl, refs.Format(p))
		m.msg = fmt.Sprintf("đoạn dán dài (%d dòng, %d ký tự) được lưu thành tệp đính kèm, agent đọc đủ nguyên văn",
			strings.Count(text, "\n")+1, len([]rune(text)))
		return m
	}
	switch fl.kind {
	case fieldArea:
		fl.area.InsertString(text)
	case fieldText:
		fl.input.SetValue(fl.input.Value() + strings.ReplaceAll(text, "\n", " "))
		fl.input.CursorEnd()
	}
	return m
}
