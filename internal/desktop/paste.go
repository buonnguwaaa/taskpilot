package desktop

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A terminal pastes text only: an image on the clipboard (a screenshot) or
// files copied in Explorer never reach a TUI. PasteFiles reads them from the
// clipboard itself.

// ErrClipboardEmpty is returned when the clipboard holds neither an image
// nor files.
var ErrClipboardEmpty = errors.New("clipboard has no image or file (copy a screenshot or files in Explorer first)")

// psPaste prints "F" and the copied files, "I" and a PNG in base64, or "N".
const psPaste = `[Console]::OutputEncoding = [Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$f = [System.Windows.Forms.Clipboard]::GetFileDropList()
if ($f.Count -gt 0) { "F"; $f; exit }
$i = [System.Windows.Forms.Clipboard]::GetImage()
if ($i) { $ms = New-Object IO.MemoryStream; $i.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png); "I"; [Convert]::ToBase64String($ms.ToArray()); exit }
"N"`

// PasteFiles returns the files on the clipboard as local paths: the files
// copied in Explorer, or the image saved as a PNG in dir.
func PasteFiles(ctx context.Context, dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if WSL() {
		if p, err := exec.LookPath("powershell.exe"); err == nil {
			out, err := exec.CommandContext(ctx, p, "-NoProfile", "-NonInteractive", "-STA", "-Command", psPaste).Output()
			if err != nil {
				return nil, fmt.Errorf("read the Windows clipboard: %w", err)
			}
			return parsePaste(ctx, string(out), dir)
		}
	}
	for _, c := range [][]string{{"wl-paste", "--type", "image/png"}, {"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}} {
		if p, err := exec.LookPath(c[0]); err == nil {
			png, err := exec.CommandContext(ctx, p, c[1:]...).Output()
			if err != nil || len(png) == 0 {
				return nil, ErrClipboardEmpty
			}
			path, err := savePNG(png, dir)
			if err != nil {
				return nil, err
			}
			return []string{path}, nil
		}
	}
	return nil, errors.New("no clipboard tool (tried powershell.exe, wl-paste, xclip)")
}

func parsePaste(ctx context.Context, out, dir string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	var rest []string
	for _, l := range lines[1:] {
		if l = strings.TrimSpace(l); l != "" {
			rest = append(rest, l)
		}
	}
	switch strings.TrimSpace(lines[0]) {
	case "F":
		var paths []string
		for _, w := range rest {
			paths = append(paths, linuxPath(ctx, w))
		}
		return paths, nil
	case "I":
		png, err := base64.StdEncoding.DecodeString(strings.Join(rest, ""))
		if err != nil {
			return nil, fmt.Errorf("clipboard image: %w", err)
		}
		path, err := savePNG(png, dir)
		if err != nil {
			return nil, err
		}
		return []string{path}, nil
	}
	return nil, ErrClipboardEmpty
}

// linuxPath maps a Windows path to WSL (C:\a → /mnt/c/a).
func linuxPath(ctx context.Context, w string) string {
	if out, err := exec.CommandContext(ctx, "wslpath", "-u", w).Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	return w // refs.Resolve also understands Windows paths
}

func savePNG(png []byte, dir string) (string, error) {
	if !bytes.HasPrefix(png, []byte("\x89PNG")) {
		return "", ErrClipboardEmpty
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "clipboard-"+time.Now().Format("20060102-150405.000")+".png")
	return path, os.WriteFile(path, png, 0o644)
}

// PasteText returns the text on the clipboard (for a TUI that receives
// Ctrl+V itself instead of the terminal pasting).
func PasteText(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if WSL() {
		if p, err := exec.LookPath("powershell.exe"); err == nil {
			out, err := exec.CommandContext(ctx, p, "-NoProfile", "-NonInteractive", "-Command",
				"[Console]::OutputEncoding = [Text.Encoding]::UTF8; Get-Clipboard -Raw").Output()
			if err != nil {
				return "", fmt.Errorf("read the Windows clipboard: %w", err)
			}
			return strings.TrimSuffix(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n"), nil
		}
	}
	for _, c := range [][]string{{"wl-paste", "--no-newline"}, {"xclip", "-selection", "clipboard", "-o"}, {"pbpaste"}} {
		if p, err := exec.LookPath(c[0]); err == nil {
			out, err := exec.CommandContext(ctx, p, c[1:]...).Output()
			return string(out), err
		}
	}
	return "", errors.New("no clipboard tool (tried powershell.exe, wl-paste, xclip, pbpaste)")
}
