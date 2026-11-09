// Package desktop opens URLs and writes the clipboard on the operator's
// machine, including WSL (Windows browser and clipboard).
package desktop

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// WSL reports whether we run under Windows Subsystem for Linux.
func WSL() bool {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// OpenURL opens url in the default browser.
func OpenURL(url string) error {
	if WSL() {
		if p, err := exec.LookPath("explorer.exe"); err == nil {
			// explorer.exe exits 1 even when it opened the URL; only a failure to start matters.
			return exec.Command(p, url).Start()
		}
	}
	for _, c := range []string{"wslview", "xdg-open", "open"} {
		if p, err := exec.LookPath(c); err == nil {
			return exec.Command(p, url).Start()
		}
	}
	return errors.New("no way to open a browser (tried explorer.exe, wslview, xdg-open, open)")
}

// Copy puts text on the clipboard.
func Copy(text string) error {
	if WSL() {
		if p, err := exec.LookPath("clip.exe"); err == nil {
			cmd := exec.Command(p)
			cmd.Stdin = bytes.NewReader(UTF16LEWithBOM(text))
			return cmd.Run()
		}
	}
	for _, c := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"pbcopy"}} {
		if p, err := exec.LookPath(c[0]); err == nil {
			cmd := exec.Command(p, c[1:]...)
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
	}
	return errors.New("no clipboard tool (tried clip.exe, wl-copy, xclip, pbcopy)")
}

// UTF16LEWithBOM encodes text the way clip.exe reads non-ASCII correctly.
func UTF16LEWithBOM(text string) []byte {
	units := utf16.Encode([]rune(strings.ReplaceAll(text, "\n", "\r\n")))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}
