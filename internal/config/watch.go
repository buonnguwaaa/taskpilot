package config

import (
	"os"
	"sync"
	"time"
)

// Watcher reloads a config file when it changes on disk, so a long-running
// process (daemon, TUI) picks up an added repo or project without a restart.
type Watcher struct {
	mu      sync.Mutex
	path    string
	modTime time.Time
	size    int64
	failed  time.Time // modTime of the last version that did not load
}

// NewWatcher watches the file c was loaded from.
func NewWatcher(c *Config) *Watcher {
	w := &Watcher{path: c.Path}
	if fi, err := os.Stat(c.Path); err == nil {
		w.modTime, w.size = fi.ModTime(), fi.Size()
	}
	return w
}

// Check returns the new config when the file changed since the last check
// and loads; nil when unchanged. A version that fails to load is reported
// once (err) and the caller keeps its current config.
func (w *Watcher) Check() (*Config, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fi, err := os.Stat(w.path)
	if err != nil || fi.ModTime().Equal(w.modTime) && fi.Size() == w.size {
		return nil, nil // missing for a moment (editor rename) or unchanged
	}
	c, err := Load(w.path)
	if err != nil {
		if fi.ModTime().Equal(w.failed) {
			return nil, nil
		}
		w.failed = fi.ModTime()
		return nil, err
	}
	w.modTime, w.size, w.failed = fi.ModTime(), fi.Size(), time.Time{}
	return c, nil
}
