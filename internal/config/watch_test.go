package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(s string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, at, at)
	}
	base := time.Now().Add(-time.Hour)
	write("projects:\n  p:\n    repos:\n      a: {path: /tmp/a}\n", base)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWatcher(c)
	if c2, err := w.Check(); c2 != nil || err != nil {
		t.Fatal("unchanged file: no reload")
	}
	write("projects:\n  p:\n    repos:\n      a: {path: /tmp/a}\n      b: {path: /tmp/b}\n", base.Add(time.Minute))
	c2, err := w.Check()
	if err != nil || c2 == nil || len(c2.Projects["p"].Repos) != 2 {
		t.Fatalf("added repo not reloaded: %v %v", c2, err)
	}
	write("projects: [broken", base.Add(2*time.Minute))
	if c3, err := w.Check(); c3 != nil || err == nil {
		t.Fatal("a broken config must be reported and not returned")
	}
	if c3, err := w.Check(); c3 != nil || err != nil {
		t.Fatal("the same broken version is reported once")
	}
}
