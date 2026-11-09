// Package plugin embeds the Claude Code plugin (skills) shipped with taskpilot
// and writes it to disk so sessions can load it with --plugin-dir.
package plugin

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:files
var files embed.FS

// Materialize writes the plugin to <home>/plugin, replacing older copies,
// and returns the directory to pass to --plugin-dir.
func Materialize(home string) (string, error) {
	dst := filepath.Join(home, "plugin")
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	err := fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("files", p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	return dst, err
}

// Read returns an embedded plugin file, e.g. "skills/prod-logs/payment.md".
func Read(path string) (string, error) {
	b, err := files.ReadFile("files/" + path)
	return string(b), err
}
