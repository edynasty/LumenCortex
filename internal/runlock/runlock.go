// Package runlock is the cross-process run lock: one agent run per session
// across every process and every Engine that opens the same state directory.
//
// The lock is a kernel advisory lock on <dir>/<session-id>.lock that belongs to
// the open file description (flock(2) on Unix, LockFileEx on Windows). A second
// open of the same file conflicts even inside one process, and the kernel drops
// the lock when the descriptor closes or the process dies (kill -9). POSIX
// fcntl record locks are not acceptable: they belong to the process, so a second
// Engine in the same process would acquire the same lock.
//
// The file content (pid and time) is diagnostic only; the kernel lock is the truth.
package runlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ErrHeld reports that another open file description, in this process or another,
// holds the lock.
var ErrHeld = errors.New("runlock: lock is held")

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Lock is a held run lock.
type Lock struct{ f *os.File }

// TryAcquire takes the lock for id without blocking. It returns ErrHeld when it
// is held, and a validation error for an id outside ^[A-Za-z0-9_-]{1,128}$.
func TryAcquire(dir, id string) (*Lock, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("runlock: invalid id %q", id)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("pid=%d at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the descriptor. It is safe to call more than once.
// The lock file stays on disk; Remove deletes it.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return errors.Join(unlock(f), f.Close())
}

// Remove deletes the lock file of id after its session has been deleted. A holder
// that raced the deletion finds no session to run. A missing file is not an error.
func Remove(dir, id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("runlock: invalid id %q", id)
	}
	err := os.Remove(filepath.Join(dir, id+".lock"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
