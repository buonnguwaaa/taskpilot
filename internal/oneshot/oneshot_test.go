package oneshot

import (
	"strings"
	"testing"
)

func TestAnswer(t *testing.T) {
	for out, want := range map[string]string{
		`{"is_error":false,"structured_output":{"a":1}}`:                                   `{"a":1}`,
		`{"is_error":false,"result":"Here:\n` + "```json" + `\n{\"a\":2}\n` + "```" + `"}`: `{"a":2}`,
	} {
		raw, _, err := Answer([]byte(out))
		if err != nil || string(raw) != want {
			t.Errorf("%s → %s %v", out, raw, err)
		}
	}
	if _, _, err := Answer([]byte(`{"is_error":true,"subtype":"error_max_turns","result":"x"}`)); err == nil || !strings.Contains(err.Error(), "error_max_turns") {
		t.Errorf("is_error: %v", err)
	}
	if got := Args("S", "P", "sonnet", ""); strings.Join(got, " ") != "-p --output-format json --json-schema S --system-prompt P --tools  --no-session-persistence --disable-slash-commands --model sonnet" {
		t.Errorf("args = %q", got)
	}
}
