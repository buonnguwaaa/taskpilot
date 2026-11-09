package desktop

import (
	"bytes"
	"testing"
)

func TestUTF16LEWithBOM(t *testing.T) {
	got := UTF16LEWithBOM("ả\n")
	want := []byte{0xFF, 0xFE, 0xA3, 0x1E, '\r', 0, '\n', 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x want % x", got, want)
	}
}
