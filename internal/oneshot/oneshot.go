// Package oneshot runs one `claude -p` turn with no tools that answers in
// JSON (drafting a ticket or a work log, writing SPL, grouping errors).
package oneshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Args is the command line of one turn: no tools, no session, the answer
// constrained to schema.
func Args(schema, system, model, settingSources string) []string {
	args := []string{"-p", "--output-format", "json", "--json-schema", schema, "--system-prompt", system,
		"--tools", "", "--no-session-persistence", "--disable-slash-commands"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if settingSources != "" {
		args = append(args, "--setting-sources", settingSources)
	}
	return args
}

// Exec runs bin with stdin from a neutral directory (no project CLAUDE.md or
// settings of wherever taskpilot runs). timeout only names the limit in the
// error; ctx carries it.
func Exec(ctx context.Context, bin string, args []string, stdin string, timeout time.Duration) ([]byte, error) {
	if bin == "" {
		return nil, errors.New("claude.bin is not set")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = os.TempDir()
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	Note(ctx, out)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("claude quá thời gian (%s)", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("claude: %w: %s", err, Clip(msg, 300))
	}
	return out, nil
}

// Answer returns the JSON answer of a run: structured_output, or the {...}
// in the result text on older CLIs. result is the text, for error messages.
func Answer(out []byte) (raw json.RawMessage, result string, err error) {
	var res struct {
		IsError          bool            `json:"is_error"`
		Subtype          string          `json:"subtype"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, "", fmt.Errorf("đọc kết quả claude: %w", err)
	}
	if res.IsError {
		return nil, res.Result, fmt.Errorf("claude báo lỗi (%s): %s", res.Subtype, Clip(res.Result, 300))
	}
	raw = res.StructuredOutput
	if len(raw) == 0 || string(raw) == "null" {
		s := res.Result
		if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
			s = s[i : j+1]
		}
		raw = json.RawMessage(s)
	}
	return raw, res.Result, nil
}

// Clip trims s and cuts it to n runes.
func Clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Usage is the token usage of one call, for the Usage tab.
type Usage struct {
	At         time.Time
	Purpose    string
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	CostUSD    float64
}

// Record, when set (by the process that opened the store), is told the
// usage of every call that answered.
var Record func(Usage)

type purposeKey struct{}

// WithPurpose names what the calls made with ctx are for ("soạn task", …).
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeKey{}, purpose)
}

// ParseUsage reads the usage of a `claude -p --output-format json` answer.
func ParseUsage(out []byte) (Usage, bool) {
	var res struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &res) != nil {
		return Usage{}, false
	}
	u := res.Usage
	if u.Input+u.Output+u.CacheRead+u.CacheWrite == 0 && res.TotalCostUSD == 0 {
		return Usage{}, false
	}
	var models []string
	for m := range res.ModelUsage {
		models = append(models, m)
	}
	sort.Strings(models)
	return Usage{Model: strings.Join(models, ","), Input: u.Input, Output: u.Output,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: res.TotalCostUSD}, true
}

// Note records the usage of an answer (Exec calls it; a caller running
// claude itself calls it too).
func Note(ctx context.Context, out []byte) {
	if Record == nil || len(out) == 0 {
		return
	}
	u, ok := ParseUsage(out)
	if !ok {
		return
	}
	u.At = time.Now()
	u.Purpose, _ = ctx.Value(purposeKey{}).(string)
	Record(u)
}
