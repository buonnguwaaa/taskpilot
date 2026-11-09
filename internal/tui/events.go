package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/buonnguwaaa/taskpilot/internal/store"
)

// activity summarizes the last stream-json events of a task.
func activity(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const maxTail = 512 << 10
	if st, err := f.Stat(); err == nil && st.Size() > maxTail {
		_, _ = f.Seek(st.Size()-maxTail, io.SeekStart)
	}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		out = append(out, summarizeEvent(sc.Bytes())...)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

type rawEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

func summarizeEvent(line []byte) []string {
	var ev rawEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return []string{"● session started"}
		}
	case "result":
		return []string{fmt.Sprintf("■ turn ended (%s, $%.2f)", ev.Subtype, ev.TotalCostUSD)}
	case "assistant", "user":
		var blocks []contentBlock
		if json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return nil
		}
		var out []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if s := firstLine(b.Text); s != "" && ev.Type == "assistant" {
					out = append(out, "💬 "+s)
				}
			case "tool_use":
				out = append(out, "→ "+b.Name+" "+briefInput(b.Input))
			case "tool_result":
				if b.IsError {
					out = append(out, "✗ "+firstLine(toolResultText(b.Content)))
				}
			}
		}
		return out
	}
	return nil
}

func briefInput(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "pattern", "gate", "skill", "path"} {
		if v, ok := m[k].(string); ok {
			return firstLine(v)
		}
	}
	return ""
}

func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Text != "" {
				return b.Text
			}
		}
	}
	return string(raw)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func truncate(s string, w int) string {
	if w <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func wrap(s string, w int) string {
	return lipgloss.NewStyle().Width(max(10, w)).Render(strings.TrimSpace(s))
}

// sortedPRs orders PRs with pending ones (needing the operator) first, then by repo.
func sortedPRs(prs []*store.PullRequest) []*store.PullRequest {
	out := append([]*store.PullRequest(nil), prs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pending != out[j].Pending {
			return out[i].Pending
		}
		return out[i].Repo < out[j].Repo || out[i].Repo == out[j].Repo && out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
