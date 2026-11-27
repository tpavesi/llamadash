package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock takes a non-blocking exclusive flock on dbPath+".lock" and holds it until
// the returned lock is closed. It returns ErrLocked when another open file holds it;
// any other failure (the lock file cannot be created) is returned as it is.
func Lock(dbPath string) (*FileLock, error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) // a lock may live in a shared directory: never through a link
	if err != nil {
		return nil, fmt.Errorf("store: lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: lock: %w", err)
	}
	return &FileLock{f: f}, nil
}
