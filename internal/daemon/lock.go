package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another daemon (a TUI with an embedded daemon, or
// `taskpilot daemon`) already schedules tasks for this home.
var ErrLocked = errors.New("another taskpilot daemon is already running for this home")

// Lock takes the per-home scheduler lock; release it with the returned func.
// The lock is an advisory flock, released automatically if the process dies.
func Lock(home string) (func(), error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	f.Truncate(0)
	fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
