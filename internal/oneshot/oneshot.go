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
