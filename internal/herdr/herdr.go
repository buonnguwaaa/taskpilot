// Package herdr drives the herdr terminal workspace manager through its CLI
// (https://herdr.dev): it lets taskpilot keep its daemon in a persistent herdr
// tab and open agent sessions in herdr panes.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Inside reports whether this process runs in a herdr-managed pane.
func Inside() bool { return os.Getenv("HERDR_ENV") == "1" }

// Client runs herdr CLI commands.
type Client struct {
	Bin string
}

// New finds the herdr binary; ok is false when herdr is not installed.
func New() (*Client, bool) {
	if p, err := exec.LookPath("herdr"); err == nil {
		return &Client{Bin: p}, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "herdr")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return &Client{Bin: p}, true
		}
	}
	return nil, false
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.Bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("herdr %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

type envelope struct {
	Result struct {
		Tab struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func parse(out []byte) (*envelope, error) {
	var e envelope
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, fmt.Errorf("herdr: unexpected output: %s", strings.TrimSpace(string(out)))
	}
	if e.Error != nil {
		return nil, fmt.Errorf("herdr: %s (%s)", e.Error.Message, e.Error.Code)
	}
	return &e, nil
}

// CreateTab opens a new tab without taking focus and returns its root pane.
func (c *Client) CreateTab(ctx context.Context, label, cwd string) (tabID, paneID string, err error) {
	args := []string{"tab", "create", "--label", label, "--no-focus"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", "", err
	}
	e, err := parse(out)
	if err != nil {
		return "", "", err
	}
	if e.Result.RootPane.PaneID == "" {
		return "", "", errors.New("herdr: tab create returned no pane id")
	}
	return e.Result.Tab.TabID, e.Result.RootPane.PaneID, nil
}

// SplitRight splits the calling pane and focuses the new one.
func (c *Client) SplitRight(ctx context.Context, cwd string) (string, error) {
	args := []string{"pane", "split", "--current", "--direction", "right", "--focus"}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return "", err
	}
	e, err := parse(out)
	if err != nil {
		return "", err
	}
	if e.Result.Pane.PaneID == "" {
		return "", errors.New("herdr: pane split returned no pane id")
	}
	return e.Result.Pane.PaneID, nil
}

// Run types command into the pane's shell and presses Enter.
func (c *Client) Run(ctx context.Context, paneID, command string) error {
	out, err := c.run(ctx, "pane", "run", paneID, command)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(out))) > 0 && strings.HasPrefix(strings.TrimSpace(string(out)), "{") {
		_, err = parse(out)
	}
	return err
}

// Quote makes a POSIX shell word.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// Command joins argv into a shell command line.
func Command(argv ...string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// Tab is a herdr tab.
type Tab struct {
	ID    string `json:"tab_id"`
	Label string `json:"label"`
}

// Tabs lists the tabs of the session.
func (c *Client) Tabs(ctx context.Context) ([]Tab, error) {
	out, err := c.run(ctx, "tab", "list")
	if err != nil {
		return nil, err
	}
	var e struct {
		Result struct {
			Tabs []Tab `json:"tabs"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, fmt.Errorf("herdr: unexpected output: %s", strings.TrimSpace(string(out)))
	}
	if e.Error != nil {
		return nil, errors.New("herdr: " + e.Error.Message)
	}
	return e.Result.Tabs, nil
}

// CloseTab closes a tab and its panes.
func (c *Client) CloseTab(ctx context.Context, tabID string) error {
	_, err := c.run(ctx, "tab", "close", tabID)
	return err
}

// Agent states a pane reports (herdr pane report-agent).
const (
	AgentIdle    = "idle"
	AgentWorking = "working"
	AgentBlocked = "blocked"
)

// reportSource names taskpilot as the reporter of the agent state.
const reportSource = "taskpilot"

// ReportAgent makes the pane show as a Claude agent in state (idle, working,
// blocked) with a short message; sessionID is the Claude session, if any.
func (c *Client) ReportAgent(ctx context.Context, paneID, state, message, sessionID string) error {
	args := []string{"pane", "report-agent", paneID, "--source", reportSource, "--agent", "claude", "--state", state}
	if message != "" {
		args = append(args, "--message", message)
	}
	if sessionID != "" {
		args = append(args, "--agent-session-id", sessionID)
	}
	_, err := c.run(ctx, args...)
	return err
}

// ReportTitle sets the title herdr shows for the pane's agent.
func (c *Client) ReportTitle(ctx context.Context, paneID, title string) error {
	_, err := c.run(ctx, "pane", "report-metadata", paneID, "--source", reportSource, "--title", title)
	return err
}

// ReleaseAgent stops reporting the pane as an agent.
func (c *Client) ReleaseAgent(ctx context.Context, paneID string) error {
	_, err := c.run(ctx, "pane", "release-agent", paneID, "--source", reportSource, "--agent", "claude")
	return err
}

// WatchLabel is the label of the tab that shows a background task.
func WatchLabel(taskID string) string { return "tp " + taskID }
