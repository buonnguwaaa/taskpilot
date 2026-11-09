package logring

import (
	"fmt"
	"strings"
	"testing"
)

func TestRing(t *testing.T) {
	r := New(3)
	fmt.Fprint(r, "a\nb")
	fmt.Fprint(r, "c\nd\ne\nf")
	if got := strings.Join(r.Lines(), ","); got != "bc,d,e" {
		t.Fatalf("lines = %s (partial lines join across writes, oldest dropped)", got)
	}
}
