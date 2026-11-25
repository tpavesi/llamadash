package store

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"llamadash/internal/stats"
)

// openTest opens a store in a fresh temp dir whose flush timer is the returned channel
// (nothing flushes on a timer unless the test sends on it).
func openTest(t *testing.T, cfg config) (*Store, string, chan time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	tick := make(chan time.Time)
	if cfg.tick == nil {
		cfg.tick = tick
	}
	s, err := open(path, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path, tick
}

func reopen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := open(path, config{tick: make(chan time.Time)})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// rawDB opens the file with the driver directly, as an external process would.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// stall holds the database write lock from a second connection until the returned func runs.
func stall(t *testing.T, path string) (release func()) {
	t.Helper()
	db := rawDB(t, path)
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	return func() {
		if _, err := c.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Errorf("rollback: %v", err)
		}
		c.Close()
	}
}

func writerExec(t *testing.T, s *Store, q string) {
	t.Helper()
	err := s.do(func(ctx context.Context, c *sql.Conn) error {
		_, err := c.ExecContext(ctx, q)
		return err
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func mustFlush(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

var base = time.Date(2026, 3, 15, 12, 0, 0, 0, time.Local)

func TestOpenCreatesAndMigrates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "sub")
	path := filepath.Join(dir, "h.db")
	s, err := open(path, config{tick: make(chan time.Time)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if runtime.GOOS != "windows" {
		if m := fi.Mode().Perm(); m != 0o600 {
			t.Errorf("file mode %o, want 600", m)
		}
		di, _ := os.Stat(dir)
		if m := di.Mode().Perm(); m != 0o700 {
			t.Errorf("dir mode %o, want 700", m)
		}
	}
	var v int
	if err := s.rdb.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d, %v; want the current version", v, err)
	}
	var mode string
	if err := s.rdb.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	s.AddSample(Sample{At: base, GenRate: 7})
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	s2 := reopen(t, path)
	pts, err := s2.Series("gen_rate", base.Add(-time.Minute), base.Add(time.Minute), 1)
	if err != nil || len(pts) != 1 || pts[0].V != 7 {
		t.Fatalf("after reopen: %v, %v", pts, err)
	}
}

func TestRoundTripRequests(t *testing.T) {
	s, path, _ := openTest(t, config{})
	var in []stats.Finished
	for i := range 3 {
		f := stats.Finished{
			At:           base.Add(time.Duration(i)*time.Second + 123456789*time.Nanosecond),
			SlotID:       i,
			TaskID:       100 + i,
			PromptTokens: 1000 * (i + 1),
			CacheShare:   0.25 * float64(i),
			Generated:    50 + i,
			Duration:     time.Duration(i+1)*time.Second + 987654321*time.Nanosecond,
			TTFT:         time.Duration(i+1)*100*time.Millisecond + 555*time.Microsecond,
			GenRate:      12.5 + float64(i),
			Approx:       i == 2,
		}
		in = append(in, f)
		s.AddRequest(f)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := reopen(t, path)
	got, err := s2.RecentRequests(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	for k, want := range []stats.Finished{in[2], in[1]} {
		g := got[k]
		want.At = want.At.Truncate(time.Millisecond)
		want.Duration = want.Duration.Truncate(time.Millisecond)
		want.TTFT = want.TTFT.Truncate(time.Millisecond)
		if !g.At.Equal(want.At) {
			t.Errorf("[%d] At %v, want %v", k, g.At, want.At)
		}
		g.At = want.At
		if g != want {
			t.Errorf("[%d]\n got %+v\nwant %+v", k, g, want)
		}
	}
	if none, err := s2.RecentRequests(0); err != nil || len(none) != 0 {
		t.Errorf("RecentRequests(0) = %v, %v", none, err)
	}
}

func TestSamplesSeries(t *testing.T) {
	s, _, _ := openTest(t, config{})
	for i := range 60 {
		s.AddSample(Sample{At: base.Add(time.Duration(i) * 10 * time.Second), GenRate: float64(i)})
	}
	mustFlush(t, s)
	pts, err := s.Series("gen_rate", base, base.Add(600*time.Second), 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 6 {
		t.Fatalf("got %d points, want 6: %v", len(pts), pts)
	}
	for k, p := range pts {
		if want := 4.5 + 10*float64(k); p.V != want {
			t.Errorf("point %d = %v, want %v", k, p.V, want)
		}
		if want := base.Add(time.Duration(k) * 100 * time.Second); !p.At.Equal(want) {
			t.Errorf("point %d at %v, want %v", k, p.At, want)
		}
	}
	for _, bad := range []string{"nope", "at", "gen_rate; DROP TABLE samples", "GEN_RATE "} {
		if _, err := s.Series(bad, base, base.Add(time.Hour), 6); err == nil {
			t.Errorf("Series(%q) accepted", bad)
		}
	}
	if _, err := s.Series("gen_rate", base, base, 6); err == nil {
		t.Error("empty range accepted")
	}
	if _, err := s.Series("gen_rate", base, base.Add(time.Hour), 0); err == nil {
		t.Error("0 buckets accepted")
	}
	var n int
	if err := s.rdb.QueryRow("SELECT COUNT(*) FROM samples").Scan(&n); err != nil || n != 60 {
		t.Fatalf("samples table after bad metric: %d, %v", n, err)
	}
}

func TestSeriesGaps(t *testing.T) {
	s, _, _ := openTest(t, config{})
	for i := range 100 {
		if i < 10 || i >= 90 {
			s.AddSample(Sample{At: base.Add(time.Duration(i) * time.Second), GPUPowerW: 100})
		}
	}
	mustFlush(t, s)
	pts, err := s.Series("gpu_w", base, base.Add(100*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || !pts[0].At.Equal(base) || !pts[1].At.Equal(base.Add(90*time.Second)) {
		t.Fatalf("got %v, want points at bucket 0 and 9", pts)
	}
}

func dailyRows(t *testing.T, s *Store) map[string]Usage {
	t.Helper()
	rows, err := s.rdb.Query("SELECT day, gen_tokens, prompt_tokens, requests, gpu_wh, cpu_wh, cost FROM daily")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]Usage{}
	for rows.Next() {
		var day string
		var u Usage
		if err := rows.Scan(&day, &u.GenTokens, &u.PromptTokens, &u.Requests, &u.GPUEnergyWh, &u.CPUEnergyWh, &u.Cost); err != nil {
			t.Fatal(err)
		}
		out[day] = u
	}
	return out
}

func TestDailyAccumulatesByLocalDay(t *testing.T) {
	s, _, _ := openTest(t, config{})
	late := time.Date(2026, 3, 14, 23, 59, 50, 0, time.Local)
	early := time.Date(2026, 3, 15, 0, 0, 10, 0, time.Local)
	s.AddUsage(late, Usage{GenTokens: 1, Requests: 1})
	s.AddUsage(early, Usage{GenTokens: 2, PromptTokens: 5, GPUEnergyWh: 0.5})
	mustFlush(t, s)
	s.AddUsage(early.Add(time.Hour), Usage{GenTokens: 3, CPUEnergyWh: 0.25, Cost: 0.125})
	mustFlush(t, s)
	got := dailyRows(t, s)
	want := map[string]Usage{
		"2026-03-14": {GenTokens: 1, Requests: 1},
		"2026-03-15": {GenTokens: 5, PromptTokens: 5, GPUEnergyWh: 0.5, CPUEnergyWh: 0.25, Cost: 0.125},
	}
	if len(got) != 2 || got["2026-03-14"] != want["2026-03-14"] || got["2026-03-15"] != want["2026-03-15"] {
		t.Fatalf("daily = %+v, want %+v", got, want)
	}
}

func TestTotals(t *testing.T) {
	s, _, _ := openTest(t, config{})
	now := base
	today, month, all, err := s.Totals(now)
	if err != nil {
		t.Fatal(err)
	}
	if today.HasData || month.HasData || all.HasData {
		t.Fatalf("empty db has data: %+v %+v %+v", today, month, all)
	}
	s.AddUsage(now.Add(-time.Hour), Usage{GenTokens: 1, Requests: 1, Cost: 0.5})
	s.AddUsage(time.Date(2026, 3, 2, 9, 0, 0, 0, time.Local), Usage{GenTokens: 10, Requests: 2, Cost: 1})
	s.AddUsage(time.Date(2026, 2, 27, 9, 0, 0, 0, time.Local), Usage{GenTokens: 100, Requests: 4, Cost: 2})
	mustFlush(t, s)
	today, month, all, err = s.Totals(now)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, got Totals, gen, req int64, cost float64) {
		t.Helper()
		if !got.HasData || got.GenTokens != gen || got.Requests != req || got.Cost != cost {
			t.Errorf("%s = %+v, want gen %d req %d cost %v", name, got, gen, req, cost)
		}
	}
	check("today", today, 1, 1, 0.5)
	check("month", month, 11, 3, 1.5)
	check("all", all, 111, 7, 3.5)

	// a new month with nothing yet
	today, month, _, _ = s.Totals(time.Date(2026, 4, 1, 0, 0, 1, 0, time.Local))
	if today.HasData || month.HasData {
		t.Errorf("april: today %+v month %+v, want no data", today, month)
	}
}

func TestTariffOnlyOnChange(t *testing.T) {
	s, _, _ := openTest(t, config{})
	steps := []struct {
		price float64
		cur   string
		rows  int
	}{
		{0.30, "EUR", 1}, {0.30, "EUR", 1}, {0.35, "EUR", 2}, {0.35, "USD", 3}, {0.35, "USD", 3},
	}
	for i, st := range steps {
		if err := s.SetTariff(base.Add(time.Duration(i)*time.Minute), st.price, st.cur); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.rdb.QueryRow("SELECT COUNT(*) FROM tariff").Scan(&n); err != nil || n != st.rows {
			t.Fatalf("step %d: %d rows (%v), want %d", i, n, err, st.rows)
		}
	}
}

func TestAddNeverBlocks(t *testing.T) {
	s, path, _ := openTest(t, config{busy: 50 * time.Millisecond})
	release := stall(t, path)
	start := time.Now()
	for i := range 10_000 {
		s.AddSample(Sample{At: base.Add(time.Duration(i) * time.Millisecond), GenRate: float64(i)})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("10000 AddSample took %v", d)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("flush succeeded while the database was locked")
	}
	release()
	mustFlush(t, s)
	s.mu.Lock()
	hw := s.maxSamples
	s.mu.Unlock()
	if hw > queueSamples || hw == 0 {
		t.Errorf("sample queue high-water mark %d, want 1..%d", hw, queueSamples)
	}
	var n int
	var last float64
	if err := s.rdb.QueryRow("SELECT COUNT(*), MAX(gen_rate) FROM samples").Scan(&n, &last); err != nil {
		t.Fatal(err)
	}
	if n != queueSamples || last != 9999 {
		t.Errorf("written %d samples (newest %v), want %d ending at 9999", n, last, queueSamples)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err after recovery: %v", err)
	}
}

func TestUsageNeverLost(t *testing.T) {
	s, path, _ := openTest(t, config{busy: 50 * time.Millisecond})
	release := stall(t, path)
	start := time.Now()
	for i := range 5_000 {
		s.AddUsage(base.Add(time.Duration(i)*time.Millisecond), Usage{GenTokens: 1, PromptTokens: 2, Requests: 1, GPUEnergyWh: 0.5, CPUEnergyWh: 0.25, Cost: 0.125})
		if i%1000 == 0 {
			s.AddSample(Sample{At: base.Add(time.Duration(i) * time.Millisecond)}) // keep the writer trying
			_ = s.Flush()
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("5000 AddUsage took %v", d)
	}
	release()
	mustFlush(t, s)
	got := dailyRows(t, s)
	want := Usage{GenTokens: 5000, PromptTokens: 10000, Requests: 5000, GPUEnergyWh: 2500, CPUEnergyWh: 1250, Cost: 625}
	if len(got) != 1 || got["2026-03-15"] != want {
		t.Fatalf("daily = %+v, want one row %+v", got, want)
	}
}

func TestWriteErrorSurfaces(t *testing.T) {
	s, _, _ := openTest(t, config{})
	// Make the writer's connection read-only: portable, and every write fails like a
	// read-only database would.
	writerExec(t, s, "PRAGMA query_only = 1")
	s.AddSample(Sample{At: base, GenRate: 1})
	s.AddUsage(base, Usage{GenTokens: 3})
	if err := s.Flush(); err == nil {
		t.Fatal("flush on a read-only connection succeeded")
	}
	if s.Err() == nil {
		t.Fatal("Err() nil after a failed flush")
	}
	start := time.Now()
	for i := range 1000 {
		s.AddSample(Sample{At: base.Add(time.Duration(i+1) * time.Second)})
		s.AddRequest(stats.Finished{At: base})
		s.AddUsage(base, Usage{GenTokens: 1})
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Add* took %v with a failing writer", d)
	}
	writerExec(t, s, "PRAGMA query_only = 0")
	mustFlush(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() after recovery: %v", err)
	}
	if got := dailyRows(t, s)["2026-03-15"].GenTokens; got != 1003 {
		t.Errorf("gen tokens after retry = %d, want 1003", got)
	}
}

func TestBackoffAfterSixFailures(t *testing.T) {
	clock := base
	s, _, tick := openTest(t, config{now: func() time.Time { return clock }})
	writerExec(t, s, "PRAGMA query_only = 1")
	s.AddSample(Sample{At: base})
	attempts := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.attempts
	}
	// tick is unbuffered: a send returns once the writer took it, so after send k+1
	// tick k has been fully handled.
	for range maxFails + 3 {
		tick <- base
	}
	writerExec(t, s, "SELECT 1") // the last tick is handled too
	if got := attempts(); got != maxFails {
		t.Fatalf("attempts = %d, want %d (paused after %d failures)", got, maxFails, maxFails)
	}
	writerExec(t, s, "PRAGMA query_only = 0")
	s.mu.Lock()
	clock = clock.Add(pauseFor + time.Second)
	s.mu.Unlock()
	tick <- base
	writerExec(t, s, "SELECT 1")
	if got := attempts(); got != maxFails+1 {
		t.Fatalf("attempts after pause = %d, want %d", got, maxFails+1)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err after successful retry: %v", err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE x(a INTEGER); PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("opened a database with user_version 99")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error %q does not mention the version", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("file changed")
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Error("a -wal file was created")
	}
}

func TestOpenRejectsNonDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("opened a non-database")
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Errorf("file now %q", b)
	}
}

func TestPrune(t *testing.T) {
	s, _, _ := openTest(t, config{})
	now := base
	day := 24 * time.Hour
	s.AddSample(Sample{At: now.Add(-40 * day)})
	s.AddSample(Sample{At: now.Add(-10 * day)})
	s.AddRequest(stats.Finished{At: now.Add(-100 * day)})
	s.AddRequest(stats.Finished{At: now.Add(-10 * day)})
	s.AddUsage(now.Add(-400*day), Usage{GenTokens: 1})
	s.AddUsage(now, Usage{GenTokens: 1})
	if err := s.SetTariff(now.Add(-400*day), 0.2, "EUR"); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, s)
	if err := s.Prune(now, 30, 90); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"samples": 1, "requests": 1, "daily": 2, "tariff": 1} {
		var n int
		if err := s.rdb.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows (%v), want %d", table, n, err, want)
		}
	}
	got, _ := s.RecentRequests(10)
	if len(got) != 1 || !got[0].At.Equal(now.Add(-10*day)) {
		t.Errorf("kept requests %+v", got)
	}
}

func TestCloseFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := Open(path) // real 10s timer: Close must not wait for it
	if err != nil {
		t.Fatal(err)
	}
	s.AddSample(Sample{At: base, GenRate: 1})
	s.AddSample(Sample{At: base, GenRate: 2}) // same millisecond: the later wins
	s.AddRequest(stats.Finished{At: base, TaskID: 9})
	s.AddUsage(base, Usage{GenTokens: 4})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, path)
	var n int
	var v float64
	if err := db.QueryRow("SELECT COUNT(*), MAX(gen_rate) FROM samples").Scan(&n, &v); err != nil || n != 1 || v != 2 {
		t.Errorf("samples: %d rows, gen_rate %v (%v); want 1 row with 2", n, v, err)
	}
	if c := count(t, db, "requests"); c != 1 {
		t.Errorf("requests: %d", c)
	}
	if c := count(t, db, "daily"); c != 1 {
		t.Errorf("daily: %d", c)
	}
}

func TestCloseBoundedWhenLocked(t *testing.T) {
	s, path, _ := openTest(t, config{busy: 50 * time.Millisecond})
	release := stall(t, path)
	defer release()
	s.AddSample(Sample{At: base})
	start := time.Now()
	err := s.Close()
	if d := time.Since(start); d > closeTimeout+time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err == nil {
		t.Fatal("Close returned nil although the final flush could not write")
	}
	if err2 := s.Close(); err2 != err {
		t.Errorf("second Close = %v, want %v", err2, err)
	}
	s.AddSample(Sample{At: base}) // after Close: no panic, no block
	if err := s.Flush(); err == nil {
		t.Error("Flush after Close succeeded")
	}
}

func TestNoTextColumnsBeyondSchema(t *testing.T) {
	s, _, _ := openTest(t, config{})
	rows, err := s.rdb.Query(`SELECT m.name, p.name, p.type FROM sqlite_master m, pragma_table_info(m.name) p WHERE m.type = 'table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var text []string
	for rows.Next() {
		var tbl, col, typ string
		if err := rows.Scan(&tbl, &col, &typ); err != nil {
			t.Fatal(err)
		}
		u := strings.ToUpper(typ)
		if u == "" {
			t.Errorf("%s.%s has no declared type", tbl, col)
		}
		if strings.Contains(u, "TEXT") || strings.Contains(u, "CHAR") || strings.Contains(u, "CLOB") || strings.Contains(u, "BLOB") {
			text = append(text, tbl+"."+col)
		}
	}
	sort.Strings(text)
	if want := []string{"daily.day", "tariff.currency"}; strings.Join(text, ",") != strings.Join(want, ",") {
		t.Fatalf("text columns %v, want %v", text, want)
	}
}

func TestUsageSanitisedPerDelta(t *testing.T) {
	s, path, _ := openTest(t, config{busy: 50 * time.Millisecond})
	nan, inf := math.NaN(), math.Inf(1)
	good := Usage{GenTokens: 2, PromptTokens: 3, Requests: 1, GPUEnergyWh: 0.5, CPUEnergyWh: 0.25, Cost: 0.125}
	bad := []Usage{
		{GenTokens: 1, GPUEnergyWh: nan, CPUEnergyWh: 0.25, Cost: 0.125},
		{GenTokens: 1, GPUEnergyWh: 0.5, CPUEnergyWh: inf, Cost: math.Inf(-1)},
		{GenTokens: -5, PromptTokens: -1, Requests: -1, GPUEnergyWh: -0.5, CPUEnergyWh: -1, Cost: -2},
	}
	s.AddUsage(base, good)
	for _, u := range bad {
		s.AddUsage(base, u)
	}
	release := stall(t, path)
	if err := s.Flush(); err == nil {
		t.Fatal("flush succeeded while locked")
	}
	s.AddUsage(base, good) // merged into the put-back batch
	for _, u := range bad {
		s.AddUsage(base, u)
	}
	release()
	mustFlush(t, s)
	want := Usage{GenTokens: 8, PromptTokens: 6, Requests: 2, GPUEnergyWh: 2, CPUEnergyWh: 1, Cost: 0.5}
	if got := dailyRows(t, s)["2026-03-15"]; got != want {
		t.Fatalf("daily = %+v, want %+v", got, want)
	}
}

// A value that is not finite is no data: NULL, never 0, and never a failed batch.
func TestSampleNaNDoesNotFailBatch(t *testing.T) {
	s, _, _ := openTest(t, config{})
	s.AddSample(Sample{At: base, GenRate: math.NaN(), GPUPowerW: math.Inf(1), CPUPowerW: 3})
	s.AddSample(Sample{At: base.Add(time.Second), GenRate: 5})
	mustFlush(t, s)
	var gen, gpu, cpu sql.NullFloat64
	if err := s.rdb.QueryRow("SELECT gen_rate, gpu_w, cpu_w FROM samples WHERE at = ?", base.UnixMilli()).Scan(&gen, &gpu, &cpu); err != nil {
		t.Fatal(err)
	}
	if gen.Valid || gpu.Valid || cpu.Float64 != 3 {
		t.Errorf("sample = %v %v %v, want NULL NULL 3", gen, gpu, cpu)
	}
	if c := count(t, rawDB(t, s.path), "samples"); c != 2 {
		t.Errorf("samples written = %d, want 2", c)
	}
}

// A sample without GPU, CPU power, host or process readings is no point in Series
// for those metrics, not a 0.
func TestMissingReadingsAreNoData(t *testing.T) {
	s, _, _ := openTest(t, config{})
	nan := math.NaN()
	s.AddSample(Sample{At: base, GenRate: 5, GPUPowerW: nan, CPUPowerW: nan, GPUUtil: nan, GPUMemPct: nan,
		GPUTempC: nan, HostCPUPct: nan, ProcCPUPct: nan}) // HasHostMem, HasProcRSS false
	s.AddSample(Sample{At: base.Add(time.Minute), GenRate: 5, GPUPowerW: 300, CPUPowerW: 100, GPUUtil: 50,
		GPUMemPct: 90, GPUTempC: 60, HostCPUPct: 3, HostMemUsedKB: 1000, HasHostMem: true, ProcCPUPct: 100,
		ProcRSSKB: 2000, HasProcRSS: true})
	mustFlush(t, s)
	from, to := base.Add(-time.Second), base.Add(2*time.Minute)
	for _, m := range sampleCols {
		pts, err := s.Series(m, from, to, 2) // the first sample in bucket 0, the second in bucket 1
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		switch m {
		case "gen_rate", "prompt_rate", "processing", "queued":
			want = 2
		}
		if len(pts) != want {
			t.Errorf("%s: %d points %v, want %d", m, len(pts), pts, want)
		}
	}
}

func TestTariffCurrencyValidated(t *testing.T) {
	s, _, _ := openTest(t, config{})
	for _, bad := range []string{"EURO", "E1R", "€", "eu", "US D", "<b>", "USD\n"} {
		if err := s.SetTariff(base, 0.3, bad); err == nil {
			t.Errorf("currency %q accepted", bad)
		}
	}
	if err := s.SetTariff(base, 0.3, "eur"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTariff(base.Add(time.Minute), 0.3, "EUR"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTariff(base.Add(2*time.Minute), 0.3, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.rdb.Query("SELECT currency FROM tariff ORDER BY at")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		got = append(got, c)
	}
	if strings.Join(got, ",") != "EUR," {
		t.Errorf("tariff currencies %q, want [EUR \"\"]", got)
	}
}
