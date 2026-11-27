package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The lighting lock lives in a directory every user can write to: a link
// planted at the lock's name must not be followed.
func TestLockDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Symlink(target, filepath.Join(dir, "x.lock")); err != nil {
		t.Fatal(err)
	}
	l, err := Lock(filepath.Join(dir, "x"))
	if err == nil {
		l.Close()
		t.Error("Lock followed a symbolic link")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("Lock created the link's target")
	}
}
