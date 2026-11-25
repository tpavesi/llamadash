package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"llamadash/internal/fakeserver"
	"llamadash/internal/store"
)

// The real lock: while one holder has it, a live start only reads.
func TestRealLockMakesSecondReadOnly(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "h.db")
	held, err := store.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	s := liveRun(t, srv.URL, path)
	if !s.DBReadOnly {
		t.Fatal("a second dashboard recorded while the lock was held")
	}
	for tab, n := range dbCounts(t, path) {
		if n != 0 {
			t.Errorf("wrote %d rows to %s", n, tab)
		}
	}
	held.Close()
	if s := liveRun(t, srv.URL, path); s.DBReadOnly {
		t.Fatal("still read-only after the lock was released")
	}
}

// A read-only dashboard retries the lock: once the recorder has gone it takes over
// recording, and nothing from its read-only period is written.
func TestReadOnlyTakesOverWhenLockFrees(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "h.db")
	held, err := store.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	tick := make(chan time.Time)
	defer func(f func(time.Duration) <-chan time.Time) { retryAfter = f }(retryAfter)
	retryAfter = func(d time.Duration) <-chan time.Time {
		if d != lockRetryEvery {
			t.Errorf("retry after %v", d)
		}
		return tick
	}
	o, _, code := parseArgs([]string{"--no-gpu", "--interval", "250ms", "--url", srv.URL, "--db", path, "--kwh-price", "0.3"},
		envFn(nil), &bytes.Buffer{})
	if code != -1 {
		t.Fatalf("parseArgs: %d", code)
	}
	c, _, finish := startSession(o, envFn(nil), false)
	at := time.Now()
	poll := func(i int) bool { return c.Poll(context.Background(), at.Add(time.Duration(i)*time.Second)).DBReadOnly }
	for i := 0; i <= 15; i++ {
		if !poll(i) {
			t.Fatalf("poll %d: recording while the lock is held", i)
		}
	}
	tick <- time.Now() // the lock is still held: stays read-only
	if !poll(15) {
		t.Fatal("recording while the lock is held")
	}
	held.Close()
	tick <- time.Now()
	for deadline := time.Now().Add(5 * time.Second); poll(15); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("did not take over the lock")
		}
	}
	for i := 16; i <= 30; i++ {
		if poll(i) {
			t.Fatalf("poll %d: read-only again", i)
		}
	}
	finish()

	n := dbCounts(t, path)
	if n["samples"] != 2 || n["tariff"] != 1 || n["daily"] == 0 {
		t.Fatalf("rows %v", n)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var first int64
	if err := db.QueryRow("SELECT MIN(at) FROM samples").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < at.Add(15*time.Second).UnixMilli() {
		t.Fatalf("a sample from the read-only period: %v", time.UnixMilli(first).Sub(at))
	}
}
