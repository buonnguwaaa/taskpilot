package daemon

import (
	"errors"
	"testing"
)

func TestLockIsExclusive(t *testing.T) {
	home := t.TempDir()
	release, err := Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(home); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v, want ErrLocked", err)
	}
	release()
	again, err := Lock(home)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}
