// Package logring keeps the last N log lines in memory (for the TUI log view).
package logring

import (
	"strings"
	"sync"
)

// Ring is an io.Writer that keeps the most recent lines.
type Ring struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  string
}

// New returns a ring holding up to max lines.
func New(max int) *Ring { return &Ring{max: max} }

// Write implements io.Writer.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.part + string(p)
	parts := strings.Split(s, "\n")
	r.part = parts[len(parts)-1]
	for _, l := range parts[:len(parts)-1] {
		r.lines = append(r.lines, l)
	}
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append([]string(nil), r.lines[over:]...)
	}
	return len(p), nil
}

// Lines returns a copy of the stored lines, oldest first.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
