// Package source abstracts where tickets come from (Hasaki Work, a mock inbox, ...).
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buonnguwaaa/taskpilot/internal/config"
)

// Ticket is a unit of work pulled from a source.
type Ticket struct {
	ID string `json:"id"`
	// Source overrides the source name used in the task id (e.g. "hw" for
	// Hasaki Work tasks dispatched from the TUI into the inbox).
	Source string `json:"source,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// Project is the taskpilot project the ticket belongs to (its scope).
	Project string `json:"project"`
	// Repo optionally names the repo to open up front.
	Repo   string   `json:"repo,omitempty"`
	URL    string   `json:"url,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// Source yields tickets ready for the agent and receives progress reports.
type Source interface {
	Name() string
	// Fetch returns tickets ready to be worked on. The daemon de-duplicates by id.
	Fetch(ctx context.Context) ([]Ticket, error)
	// Report posts a progress note back to the ticket (status change, PR link, ...).
	Report(ctx context.Context, ticketID, msg string) error
}

// New builds the source named in the config.
func New(c config.Source) (Source, error) {
	switch c.Type {
	case "mock":
		return NewMock(c.Dir)
	default:
		return nil, fmt.Errorf("unknown source type %q (available: mock)", c.Type)
	}
}

// Mock reads tickets from <dir>/*.json and appends reports to <dir>/<id>.log.
// It stands in for Hasaki Work until that integration exists.
type Mock struct{ Dir string }

// NewMock creates the inbox directory if needed.
func NewMock(dir string) (*Mock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Mock{Dir: dir}, nil
}

// Name implements Source.
func (m *Mock) Name() string { return "mock" }

// Fetch implements Source.
func (m *Mock) Fetch(context.Context) ([]Ticket, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Ticket
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var t Ticket
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if t.ID == "" {
			t.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Report implements Source.
func (m *Mock) Report(_ context.Context, ticketID, msg string) error {
	f, err := os.OpenFile(filepath.Join(m.Dir, sanitize(ticketID)+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), msg)
	return err
}

// Submit writes a ticket into the mock inbox.
func (m *Mock) Submit(t Ticket) error {
	if t.ID == "" {
		t.ID = time.Now().UTC().Format("20060102-150405")
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.Dir, sanitize(t.ID)+".json"), b, 0o644)
}

func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '-'
		}
		return r
	}, filepath.Base(id))
}

// Remove deletes a ticket (and its report log) from the mock inbox, so the
// daemon does not pick it up again once its task is deleted. A missing ticket
// is not an error.
func (m *Mock) Remove(ticketID string) error {
	for _, ext := range []string{".json", ".log"} {
		if err := os.Remove(filepath.Join(m.Dir, sanitize(ticketID)+ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
