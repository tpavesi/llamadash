package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReasonFromRealErrors(t *testing.T) {
	dir := t.TempDir()

	notDB := filepath.Join(dir, "hello.db")
	if err := os.WriteFile(notDB, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(notDB)
	if !errors.Is(err, ErrNotDatabase) || Reason(err) != "not a database" {
		t.Errorf("short non-database: %v -> %q", err, Reason(err))
	}

	// A valid header over garbage gets past the header check: SQLite itself says NOTADB.
	corrupt := filepath.Join(dir, "corrupt.db")
	body := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
	for i := 16; i < len(body); i++ {
		body[i] = 0xA5
	}
	if err := os.WriteFile(corrupt, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(corrupt); err == nil {
		s.Close()
		t.Error("opened a corrupt file")
	} else if Reason(err) != "not a database" {
		t.Errorf("corrupt: %v -> %q", err, Reason(err))
	}

	newer := filepath.Join(dir, "newer.db")
	db, err := sql.Open("sqlite", newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = Open(newer)
	if !errors.Is(err, ErrNewerSchema) || Reason(err) != "newer schema" {
		t.Errorf("newer: %v -> %q", err, Reason(err))
	}
}

// garbled is a database with data on several pages whose pages after the first are
// garbage: it opens (page 1 holds the header and the schema) but reads fail.
func garbled(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "garbled.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		s.AddUsage(base.AddDate(0, 0, -i), Usage{GenTokens: int64(i + 1)})
		if i%200 == 199 {
			mustFlush(t, s)
		}
	}
	mustFlush(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 3*4096 {
		t.Fatalf("database of %d bytes: %v", len(b), err)
	}
	for i := 4096; i < len(b); i++ {
		b[i] = 0xA5
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A file damaged after page 1 opens, then fails at runtime: the reason is "corrupt".
func TestReasonCorruptAtRuntime(t *testing.T) {
	s, err := Open(garbled(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	_, _, _, err = s.Totals(base)
	if err == nil || Reason(err) != "corrupt" {
		t.Fatalf("totals: %v -> %q", err, Reason(err))
	}
	s.AddUsage(base, Usage{GenTokens: 1})
	if err := s.Flush(); err == nil || Reason(err) != "corrupt" || Reason(s.Err()) != "corrupt" {
		t.Fatalf("flush: %v -> %q", err, Reason(err))
	}
}

func TestReasonLocked(t *testing.T) {
	s, path, _ := openTest(t, config{busy: 20 * time.Millisecond})
	release := stall(t, path)
	defer release()
	s.AddUsage(base, Usage{GenTokens: 1})
	err := s.Flush()
	if err == nil {
		t.Fatal("flush under a held write lock succeeded")
	}
	if Reason(err) != "locked" || Reason(s.Err()) != "locked" {
		t.Fatalf("%v -> %q", err, Reason(err))
	}
}

func TestReasonReadOnly(t *testing.T) {
	s, _, _ := openTest(t, config{})
	writerExec(t, s, "PRAGMA query_only = 1")
	s.AddUsage(base, Usage{GenTokens: 1})
	if err := s.Flush(); Reason(err) != "permission denied" {
		t.Fatalf("%v -> %q", err, Reason(err))
	}
}

func TestReasonWrapped(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("store: %w", &fs.PathError{Op: "mkdir", Path: "/x", Err: fs.ErrPermission}), "permission denied"},
		{fmt.Errorf("store: %w", &fs.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}), "disk full"},
		{codeErr(13), "disk full"},
		{codeErr(5), "locked"},
		{codeErr(6), "locked"},
		{codeErr(5 | 2<<8), "locked"},   // an extended BUSY code
		{codeErr(26), "corrupt"},        // NOTADB at runtime: a damaged file
		{codeErr(11), "corrupt"},        // CORRUPT
		{codeErr(11 | 1<<8), "corrupt"}, // an extended CORRUPT code
		{fmt.Errorf("store: x: %w", ErrNotDatabase), "not a database"},
		{codeErr(1), "error"},
		{errors.New("database is locked"), "error"}, // text alone is never trusted
	} {
		if got := Reason(c.err); got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

type codeErr int

func (c codeErr) Error() string { return fmt.Sprintf("sqlite code %d", int(c)) }
func (c codeErr) Code() int     { return int(c) }
