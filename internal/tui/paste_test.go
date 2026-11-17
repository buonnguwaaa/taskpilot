package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/buonnguwaaa/taskpilot/internal/desktop"
	"github.com/buonnguwaaa/taskpilot/internal/store"
)

func TestAltVPastesClipboardImageIntoMessage(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-p", Title: "Sửa màn style", Project: "bom"}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()
	var gotDir string
	m.d.PasteFiles = func(_ context.Context, dir string) ([]string, error) {
		gotDir = dir
		os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, "clipboard-1.png")
		return []string{p}, os.WriteFile(p, []byte("\x89PNG fake"), 0o644)
	}
	m, _ = press(t, m, "m")
	if m.modal == nil {
		t.Fatal("m opens the message form")
	}
	m = typeText(m, "lỗi như hình")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true})
	mm, _ = mm.(Model).Update(cmd())
	m = mm.(Model)
	if !strings.Contains(m.msg, "đã dán 1 tệp") || gotDir == "" {
		t.Fatalf("msg = %q", m.msg)
	}
	m, cmd = press(t, m, "ctrl+s")
	m = drain(t, m, cmd)
	msgs, _ := st.OperatorMessages(tk.ID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "lỗi như hình") || !strings.Contains(msgs[0].Text, "clipboard-1.png") {
		t.Fatalf("message: %+v", msgs)
	}
	if _, err := os.Stat(filepath.Join(st.Home, "refs", tk.ID, "clipboard-1.png")); err != nil {
		t.Fatalf("the image is copied next to the task: %v", err)
	}
}

func TestCtrlVPastesTextWhenNoImage(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-q", Title: "x", Project: "bom"}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m.d.PasteFiles = func(context.Context, string) ([]string, error) { return nil, desktop.ErrClipboardEmpty }
	m.d.PasteText = func(context.Context) (string, error) { return "dòng 1\ndòng 2", nil }
	m, _ = press(t, m, "m")
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	mm, _ = mm.(Model).Update(cmd())
	m = mm.(Model)
	if v := m.modal.value("text"); v != "dòng 1\ndòng 2" {
		t.Fatalf("pasted %q", v)
	}
}

func TestLongPasteBecomesAFile(t *testing.T) {
	m, st, _ := newTest(t)
	tk := &store.Task{ID: "mock-l", Title: "x", Project: "bom"}
	tk.Transition(store.StateNeedsAttention, "")
	st.SaveTask(tk)
	m.reloadTasks()
	m, _ = press(t, m, "m")
	m = typeText(m, "API trả về:")
	var b strings.Builder
	b.WriteString("{\n  \"data\": [\n")
	for i := 0; i < 13000; i++ {
		b.WriteString(`    {"id": 1, "name": "Vải Single Jersey AquaX/TN095_Trang Nhã"},` + "\n")
	}
	b.WriteString("    {}\n  ]\n}")
	big := b.String()
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(big), Paste: true})
	m = mm.(Model)
	if !strings.Contains(m.msg, "đoạn dán dài") {
		t.Fatalf("msg = %q", m.msg)
	}
	small := "dòng ngắn\nthứ hai"
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(small), Paste: true})
	m = mm.(Model)
	m, cmd := press(t, m, "ctrl+s")
	m = drain(t, m, cmd)
	msgs, _ := st.OperatorMessages(tk.ID)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "dòng ngắn\nthứ hai") || !strings.Contains(msgs[0].Text, ".json") {
		t.Fatalf("message: %.300s", msgs[0].Text)
	}
	files, _ := filepath.Glob(filepath.Join(st.Home, "refs", tk.ID, "paste-*.json"))
	if len(files) != 1 {
		t.Fatalf("pasted file next to the task: %v", files)
	}
	got, _ := os.ReadFile(files[0])
	if string(got) != big {
		t.Fatalf("the file keeps every character: %d vs %d", len(got), len(big))
	}
}
