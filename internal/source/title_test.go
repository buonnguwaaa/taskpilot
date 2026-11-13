package source

import "testing"

func TestTitleFrom(t *testing.T) {
	for in, want := range map[string]string{
		"\n\n## Service nào gọi /v1/refund?\nchi tiết": "Service nào gọi /v1/refund?",
		"":    "",
		"   ": "",
	} {
		if got := TitleFrom(in); got != want {
			t.Errorf("TitleFrom(%q) = %q, want %q", in, got, want)
		}
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "a"
	}
	if r := []rune(TitleFrom(long)); len(r) != 80 || r[79] != '…' {
		t.Errorf("long title: %q", string(r))
	}
}
