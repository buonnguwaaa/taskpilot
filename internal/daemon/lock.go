package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	fmt.Fprintf(f, "pid=%d store=sqlite\n", os.Getpid())
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// LockOwner describes the process holding the scheduler lock, as it recorded
// itself. SQLite is false for daemons from before the SQLite store, which
// still write task state to files and must not run next to a new one.
func LockOwner(home string) (pid int, sqlite bool) {
	b, err := os.ReadFile(filepath.Join(home, "daemon.lock"))
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if _, err := fmt.Sscanf(s, "pid=%d", &pid); err == nil {
		return pid, strings.Contains(s, "store=sqlite")
	}
	fmt.Sscanf(s, "%d", &pid)
	return pid, false
}

// ErrOldDaemon means a daemon from before the SQLite store holds the lock.
var ErrOldDaemon = errors.New("an older taskpilot (file-based store) is running")

// CheckOwner returns ErrOldDaemon (with the pid) when the lock holder predates the SQLite store.
func CheckOwner(home string) error {
	if pid, sqlite := LockOwner(home); !sqlite {
		return fmt.Errorf("%w (pid %d): quit it (q in its TUI, or stop `taskpilot daemon`) and start taskpilot again", ErrOldDaemon, pid)
	}
	return nil
}
