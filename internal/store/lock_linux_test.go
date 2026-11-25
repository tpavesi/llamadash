package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExclusive(t *testing.T) {
	db := filepath.Join(t.TempDir(), "h.db")
	first, err := Lock(db)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(db + ".lock"); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
	if second, err := Lock(db); !errors.Is(err, ErrLocked) {
		second.Close()
		t.Fatalf("second Lock: %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Lock(db)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	third.Close()
	if _, err := os.Stat(db + ".lock"); err != nil {
		t.Fatalf("lock file deleted: %v", err)
	}
}

func TestLockUncreatable(t *testing.T) {
	_, err := Lock(filepath.Join(t.TempDir(), "missing", "h.db"))
	if err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("lock in a missing directory: %v", err)
	}
}
