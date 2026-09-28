//go:build unix

package state

// acquireLock is Store.Update's locking primitive. The unix constraint
// mirrors state_umask_test.go's: syscall.Flock exists only there, and
// Windows and plan9 are not targets this repo builds (go.mod's two platform
// files are _linux.go and _darwin.go, both unix).

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireLock opens (creating if absent) the file at path and takes an
// exclusive, non-blocking flock on it, retrying on EWOULDBLOCK until
// deadline elapses.
//
// The open is O_RDONLY|O_CREATE|O_NOFOLLOW, mode 0644, deliberately not
// O_RDWR: flock is independent of the descriptor's read/write mode, so
// O_RDONLY still locks, and 0644 denies write to a non-owner exactly as 0600
// would -- opening O_RDWR would make a root-created lock file EACCES every
// later unprivileged Update, which is the precise trap this avoids. The file
// is never written to or truncated; flock is a lock on the open file
// description, and the file's own bytes are never read. O_NOFOLLOW keeps a
// symlink planted at path from being followed into creating or locking
// something else.
//
// Each call opens a fresh file descriptor. flock locks are associated with
// the open file description and not with the process, so two different
// descriptors opened by two different Update calls in the same process
// contend for the same lock exactly as they would in two different
// processes -- which is what lets two goroutines racing Update serialise
// through this function rather than deadlock on it, and what lets a
// genuinely re-entrant call (Update invoked again from inside its own fn, in
// the same goroutine, while the outer call still holds the lock) fail with a
// timeout error rather than hang forever.
func acquireLock(path string, deadline, retryInterval time.Duration) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	giveUp := time.Now().Add(deadline)
	for {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr == nil {
			break
		}
		if !errors.Is(flockErr, syscall.EWOULDBLOCK) || time.Now().After(giveUp) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s within %s: %w", path, deadline, flockErr)
		}
		time.Sleep(retryInterval)
	}
	return func() error {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		if flockErr != nil {
			return fmt.Errorf("unlock %s: %w", path, flockErr)
		}
		return closeErr
	}, nil
}
