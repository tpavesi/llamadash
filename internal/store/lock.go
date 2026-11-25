package store

import (
	"errors"
	"os"
)

// ErrLocked means another process holds the database's lock file: it is recording,
// so this one must not write.
var ErrLocked = errors.New("store: another process holds the database lock")

// FileLock is an exclusive lock on <database>.lock, held until Close. The lock file
// is created 0600 and never deleted.
type FileLock struct{ f *os.File }

// Close releases the lock. It is safe on a nil lock and on one that holds no file.
func (l *FileLock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close() // closing the descriptor releases the flock
}
