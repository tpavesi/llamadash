package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/fakeserver"
	"llamadash/internal/gpu"
	"llamadash/internal/stats"
	"llamadash/internal/store" // also registers the sqlite driver for the checks below
)

// Tests never run as root here: the default database path then comes from the
// (empty) test environment and never touches /var/lib.
func TestMain(m *testing.M) {
	geteuid = func() int { return 1000 }
	ledLockDir = filepath.Join(os.TempDir(), "llamadash-test-no-such-dir") // never the real /run/lock
	// nor do they read the machine's own /etc/llamadash.conf
	readFile = func(p string) ([]byte, error) {
		if p == systemConfig {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return os.ReadFile(p)
	}
	os.Exit(m.Run())
}

func runArgs(args []string, env map[string]string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func TestOnceExitCodes(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	code, out, errs := runArgs([]string{"--once", "--no-gpu", "--interval", "250ms", "--url", srv.URL}, nil)
	if code != 0 || !strings.Contains(out, "example-model-27b") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errs)
	}

	dead := fakeserver.New(nil)
	url := dead.URL
	dead.Close()
	code, out, _ = runArgs([]string{"--once", "--no-gpu", "--interval", "250ms", "--url", url}, nil)
	if code != 1 || !strings.Contains(out, "unreachable") {
		t.Fatalf("closed port: code=%d out=%s", code, out)
	}

	if code, _, errs = runArgs([]string{"--once", "--interval", "1ms"}, nil); code != 2 || errs == "" {
		t.Fatalf("interval: code=%d err=%q", code, errs)
	}
	if code, _, _ = runArgs([]string{"--once", "--interval", "11s"}, nil); code != 2 {
		t.Fatalf("interval 11s: code=%d", code)
	}
	if code, _, _ = runArgs([]string{"--gpu", "--no-gpu"}, nil); code != 2 {
		t.Fatalf("gpu flags: code=%d", code)
	}
	if code, _, _ = runArgs([]string{"--bogus"}, nil); code != 2 {
		t.Fatalf("bogus flag: code=%d", code)
	}
}

func TestEnvAndFlagPrecedence(t *testing.T) {
	good := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer good.Close()
	dead := fakeserver.New(nil)
	deadURL := dead.URL
	dead.Close()
	base := []string{"--once", "--no-gpu", "--interval", "250ms"}

	code, out, _ := runArgs(base, map[string]string{"LLAMADASH_URL": good.URL})
	if code != 0 || !strings.Contains(out, "example-model-27b") {
		t.Fatalf("env url: code=%d", code)
	}
	code, _, _ = runArgs(append([]string{"--url", deadURL}, base...), map[string]string{"LLAMADASH_URL": good.URL})
	if code != 1 {
		t.Fatalf("flag must beat env: code=%d", code)
	}
}

func TestAPIKeyPrecedence(t *testing.T) {
	cfg, _, code := parseArgs([]string{}, func(k string) string { return map[string]string{"LLAMA_API_KEY": "envkey"}[k] }, &bytes.Buffer{})
	if code != -1 || cfg.apiKey != "envkey" || cfg.url != "http://localhost:8080" {
		t.Fatalf("%+v %d", cfg, code)
	}
	cfg, _, _ = parseArgs([]string{"--api-key", "flagkey"}, envFn(map[string]string{"LLAMA_API_KEY": "envkey"}), &bytes.Buffer{})
	if cfg.apiKey != "flagkey" {
		t.Fatalf("%+v", cfg)
	}
}

func TestVersionAndNotATerminal(t *testing.T) {
	code, out, _ := runArgs([]string{"--version"}, nil)
	if code != 0 || !strings.Contains(out, version) {
		t.Fatalf("%d %q", code, out)
	}
	code, out, errs := runArgs(nil, nil)
	if code != 2 || !strings.Contains(errs, "not a terminal: use --once") || out != "" {
		t.Fatalf("%d out=%q err=%q", code, out, errs)
	}
}

func TestFrame(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	base := []string{"--no-gpu", "--interval", "250ms", "--url", srv.URL}
	code, out, errs := runArgs(append([]string{"--frame", "135x40"}, base...), map[string]string{"TERM": "linux"})
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errs)
	}
	ls := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(ls) != 40 || !strings.Contains(ls[0], "135x40") || !strings.Contains(out, "example-model-27b") ||
		!strings.Contains(out, "Requests") || !strings.Contains(out, "generating") {
		t.Fatalf("%d lines:\n%s", len(ls), out)
	}
	if strings.ContainsAny(out, "▁▂▃▄▅▆▇") {
		t.Fatalf("TERM=linux frame has sparkline glyphs:\n%s", out)
	}

	_, out, _ = runArgs(append([]string{"--frame", "80x24", "--ascii"}, base...), map[string]string{"NO_COLOR": "1"})
	for _, r := range out {
		if r > 127 || r == 0x1b {
			t.Fatalf("--ascii with NO_COLOR: %q in\n%s", r, out)
		}
	}

	dead := fakeserver.New(nil)
	deadURL := dead.URL
	dead.Close()
	if code, out, _ = runArgs([]string{"--frame", "80x24", "--no-gpu", "--interval", "250ms", "--url", deadURL}, nil); code != 1 ||
		!strings.Contains(out, "unreachable") {
		t.Fatalf("unreachable: code=%d\n%s", code, out)
	}

	for _, bad := range []string{"135", "0x10", "x", "10x-1", "axb", "135x120x2"} {
		if code, _, errs := runArgs([]string{"--frame", bad}, nil); code != 2 || errs == "" {
			t.Errorf("--frame %q: code=%d err=%q", bad, code, errs)
		}
	}
	if code, _, _ := runArgs([]string{"--frame", "80x24", "--once"}, nil); code != 2 {
		t.Errorf("--frame with --once: code=%d", code)
	}
}

func TestAPIKeyNotLeakedToStderr(t *testing.T) {
	env := map[string]string{"LLAMA_API_KEY": "SECRETKEY123"}
	for _, args := range [][]string{{"--bogus"}, {"-h"}, {"--gpu", "--no-gpu"}} {
		_, out, errs := runArgs(args, env)
		if strings.Contains(errs, "SECRETKEY123") || strings.Contains(out, "SECRETKEY123") {
			t.Fatalf("%v leaked key: %s", args, errs)
		}
	}
}

func TestURLValidation(t *testing.T) {
	for _, bad := range []string{"localhost:8011", "10.10.0.5:8011", "ftp://x", "", "http://", "http://:8011"} {
		code, out, errs := runArgs([]string{"--once", "--url", bad}, nil)
		if code != 2 || out != "" || !strings.Contains(errs, "--url must look like http://host:port") {
			t.Errorf("--url %q: code=%d out=%q err=%q", bad, code, out, errs)
		}
	}
	if code, _, errs := runArgs([]string{"--once"}, map[string]string{"LLAMADASH_URL": "localhost:8011"}); code != 2 ||
		!strings.Contains(errs, "--url must look like") {
		t.Errorf("LLAMADASH_URL: code=%d err=%q", code, errs)
	}
	for _, good := range []string{"http://localhost:8011", "https://10.10.0.5:8011/", "http://[::1]:8080"} {
		if _, _, code := parseArgs([]string{"--url", good}, func(string) string { return "" }, &bytes.Buffer{}); code != -1 {
			t.Errorf("--url %q rejected: code=%d", good, code)
		}
	}
}

// --once and --frame poll twice; the GPU query of the first poll runs in the
// background, and the second poll must still show its result.
func TestTwoPollsSeesGPU(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	c := collect.New(collect.Config{URL: srv.URL, Interval: 250 * time.Millisecond, GPU: collect.GPUOn,
		GPUQuery: func(context.Context) ([]gpu.GPU, error) {
			time.Sleep(40 * time.Millisecond) // slower than the interval passed below
			return []gpu.GPU{{Index: 0, Name: "fake", PowerW: 100, HasPower: true}}, nil
		},
		CPUPower: func(time.Time) (float64, float64, bool, error) { return 0, 0, false, nil },
	})
	s := twoPolls(c, time.Millisecond)
	if len(s.GPUs) != 1 || !s.HasGPUPower {
		t.Fatalf("second poll has no GPU data: %+v", s.GPUs)
	}
}

func envFn(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPriceAndCurrencyFlags(t *testing.T) {
	o, _, code := parseArgs([]string{"--kwh-price", "0.3031", "--currency", "gbp"}, envFn(nil), &bytes.Buffer{})
	if code != -1 || o.kwhPrice != 0.3031 || o.currency != "GBP" {
		t.Fatalf("flags: %+v code %d", o, code)
	}
	o, _, code = parseArgs(nil, envFn(map[string]string{"LLAMADASH_KWH_PRICE": "0.25", "LLAMADASH_CURRENCY": "eur"}), &bytes.Buffer{})
	if code != -1 || o.kwhPrice != 0.25 || o.currency != "EUR" {
		t.Fatalf("env: %+v code %d", o, code)
	}
	o, _, code = parseArgs([]string{"--kwh-price", "0.5", "--currency", "usd"},
		envFn(map[string]string{"LLAMADASH_KWH_PRICE": "0.25", "LLAMADASH_CURRENCY": "eur"}), &bytes.Buffer{})
	if code != -1 || o.kwhPrice != 0.5 || o.currency != "USD" {
		t.Fatalf("flag beats env: %+v code %d", o, code)
	}
	if o, _, code = parseArgs(nil, envFn(nil), &bytes.Buffer{}); code != -1 || o.kwhPrice != 0 || o.currency != "" {
		t.Fatalf("default: %+v code %d", o, code)
	}
	for _, args := range [][]string{
		{"--kwh-price", "0"}, {"--kwh-price", "abc"}, {"--kwh-price", "-1"}, {"--kwh-price", "NaN"},
		{"--currency", "POUNDS"}, {"--currency", "G1P"}, {"--currency", "£"},
	} {
		var errb bytes.Buffer
		if _, _, code := parseArgs(args, envFn(nil), &errb); code != 2 || errb.Len() == 0 {
			t.Errorf("%v: code %d err %q", args, code, errb.String())
		}
	}
	for _, env := range []map[string]string{{"LLAMADASH_KWH_PRICE": "abc"}, {"LLAMADASH_CURRENCY": "POUNDS"}} {
		if _, _, code := parseArgs(nil, envFn(env), &bytes.Buffer{}); code != 2 {
			t.Errorf("env %v: code %d", env, code)
		}
	}
}

// dbCounts returns the row count of each table, opened as an external reader.
func dbCounts(t *testing.T, path string) map[string]int {
	t.Helper()
	// a busy timeout: a background read of the run just finished may still be closing
	// its connection (the last one checkpoints the WAL)
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]int{}
	for _, tab := range []string{"samples", "requests", "daily", "tariff"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + tab).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tab, err)
		}
		out[tab] = n
	}
	return out
}

func TestDBFlags(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "h.db")
	base := []string{"--no-gpu", "--interval", "250ms", "--url", srv.URL, "--kwh-price", "0.3", "--currency", "GBP"}
	for _, mode := range [][]string{{"--once"}, {"--frame", "80x24"}} {
		code, out, errs := runArgs(append(append(mode, base...), "--db", path), nil)
		if code != 0 || strings.Contains(out, "db: off") {
			t.Fatalf("%v: code %d err %s\n%s", mode, code, errs, out)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created: %v", err)
	}
	for tab, n := range dbCounts(t, path) {
		if n != 0 {
			t.Errorf("--once/--frame wrote %d rows to %s", n, tab)
		}
	}
	envPath := filepath.Join(dir, "env.db")
	if code, out, _ := runArgs(append([]string{"--once"}, base...), map[string]string{"LLAMADASH_DB": envPath}); code != 0 ||
		strings.Contains(out, "db: off") {
		t.Fatalf("LLAMADASH_DB: code %d\n%s", code, out)
	}
	if code, out, _ := runArgs(append([]string{"--once", "--no-db"}, base...), map[string]string{"LLAMADASH_DB": filepath.Join(dir, "none.db")}); code != 0 ||
		strings.Contains(out, "db:") {
		t.Fatalf("--no-db beats LLAMADASH_DB: code %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "none.db")); err == nil {
		t.Error("--no-db created the database")
	}
	for _, args := range [][]string{
		{"--db", path, "--no-db"}, {"--retain-days", "0"}, {"--retain-days", "3651"}, {"--retain-days", "x"},
	} {
		if code, _, _ := runArgs(append([]string{"--once"}, args...), nil); code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if o, _, code := parseArgs(nil, envFn(nil), &bytes.Buffer{}); code != -1 || o.retainDays != 30 {
		t.Errorf("default retain: %+v", o)
	}
	if o, _, code := parseArgs([]string{"--retain-days", "3650"}, envFn(nil), &bytes.Buffer{}); code != -1 || o.retainDays != 3650 {
		t.Errorf("retain 3650: %+v", o)
	}
}

func TestFrameWithRange(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "h.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := range 360 { // an hour of samples, one per 10 s
		st.AddSample(store.Sample{At: now.Add(-time.Duration(i) * 10 * time.Second), GenRate: 48})
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	base := []string{"--frame", "135x60", "--no-gpu", "--interval", "250ms", "--url", srv.URL, "--db", path}
	noColor := map[string]string{"NO_COLOR": "1"}
	code, out, errs := runArgs(append(base, "--range", "1h"), noColor)
	if code != 0 {
		t.Fatalf("code %d err %s", code, errs)
	}
	ls := strings.Split(out, "\n")
	title, bottom := "", ""
	for i, l := range ls {
		if strings.Contains(l, "┌ generation tok/s") {
			title = l
		}
		if title != "" && bottom == "" && strings.Contains(l, "└-1h") {
			bottom = ls[i-1] // the chart's bottom row: the history's 48 tok/s
		}
	}
	if !strings.Contains(title, "1h") || strings.Contains(title, "unavailable") || strings.Contains(title, "loading") ||
		!strings.Contains(ls[0], "range 1h") || !strings.Contains(bottom, "██████") {
		t.Fatalf("chart title %q:\n%s", title, out)
	}
	if code, _, errs := runArgs(append(base, "--range", "2h"), nil); code != 2 || !strings.Contains(errs, "--range") {
		t.Fatalf("--range 2h: code %d err %q", code, errs)
	}
	// no database: the frame says so instead of drawing nothing
	code, out, _ = runArgs([]string{"--frame", "135x60", "--no-gpu", "--interval", "250ms", "--url", srv.URL, "--no-db", "--range", "24h"}, noColor)
	if code != 0 || !strings.Contains(out, "history unavailable") {
		t.Fatalf("no db: code %d\n%s", code, out)
	}
}

// liveRun starts the live dashboard's session on path (without a terminal), polls
// for 12 s of collector time (past one 10 s window) and finishes it.
func liveRun(t *testing.T, srvURL, path string) collect.Snapshot {
	t.Helper()
	o, _, code := parseArgs([]string{"--no-gpu", "--interval", "250ms", "--url", srvURL, "--db", path, "--kwh-price", "0.3"},
		envFn(nil), &bytes.Buffer{})
	if code != -1 {
		t.Fatalf("parseArgs: %d", code)
	}
	c, _, finish := startSession(o, envFn(nil), false)
	var s collect.Snapshot
	at := time.Now()
	for i := 0; i <= 12; i++ {
		s = c.Poll(context.Background(), at.Add(time.Duration(i)*time.Second))
	}
	finish()
	return s
}

// A second live dashboard on a database another one is recording to only reads it.
func TestSecondDashboardIsReadOnly(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "h.db")
	defer func(f func(string) (*store.FileLock, error)) { lockDB = f }(lockDB)
	lockDB = func(string) (*store.FileLock, error) { return nil, store.ErrLocked }
	s := liveRun(t, srv.URL, path)
	if !s.DBReadOnly || s.DBStatus != "" {
		t.Fatalf("DBReadOnly %v DBStatus %q", s.DBReadOnly, s.DBStatus)
	}
	for tab, n := range dbCounts(t, path) {
		if n != 0 {
			t.Errorf("the read-only dashboard wrote %d rows to %s", n, tab)
		}
	}
	// the same run holding the lock records
	lockDB = func(string) (*store.FileLock, error) { return &store.FileLock{}, nil }
	s = liveRun(t, srv.URL, path)
	n := dbCounts(t, path)
	if s.DBReadOnly || n["samples"] == 0 || n["daily"] == 0 || n["tariff"] != 1 {
		t.Fatalf("recording run: DBReadOnly %v rows %v", s.DBReadOnly, n)
	}
	// a lock file that cannot be created: as if the database did not open
	lockDB = func(string) (*store.FileLock, error) { return nil, fmt.Errorf("store: lock: %w", os.ErrPermission) }
	if s = liveRun(t, srv.URL, path); s.DBStatus != "off (permission denied)" || s.DBReadOnly {
		t.Fatalf("lock failure: DBStatus %q DBReadOnly %v", s.DBStatus, s.DBReadOnly)
	}
}

// A hangup (a dropped SSH session) quits the program the normal way, so the exit
// path flushes the last window and closes the database.
func TestHangupQuits(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	quit := make(chan struct{}, 1)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() { quitOnSignal(sigs, func() { quit <- struct{}{} }, done); close(returned) }()
	sigs <- syscall.SIGHUP
	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("a hangup did not quit")
	}
	close(done)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("did not return once the program ended")
	}
}

// A database damaged after its first page opens, then fails to read: the
// dashboard keeps running and says "db: off (corrupt)".
func TestCorruptDatabaseIsNotFatal(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "h.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		st.AddUsage(time.Now().AddDate(0, 0, -i), store.Usage{GenTokens: int64(i + 1)})
		if i%200 == 199 {
			if err := st.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 3*4096 {
		t.Fatalf("%d bytes: %v", len(b), err)
	}
	for i := 4096; i < len(b); i++ {
		b[i] = 0xA5
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runArgs([]string{"--once", "--no-gpu", "--interval", "250ms", "--url", srv.URL, "--db", path}, nil)
	if code != 0 || !strings.Contains(out, "db: off (corrupt)") || !strings.Contains(out, "example-model-27b") {
		t.Fatalf("code %d err %s\n%s", code, errs, out)
	}
}

func TestDBOpenFailureIsNotFatal(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "hello.db")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runArgs([]string{"--once", "--no-gpu", "--interval", "250ms", "--url", srv.URL, "--db", path}, nil)
	if code != 0 || !strings.Contains(out, "db: off (not a database)") {
		t.Fatalf("code %d err %s\n%s", code, errs, out)
	}
	if b, _ := os.ReadFile(path); string(b) != "hello" {
		t.Fatalf("file now %q", b)
	}
}

func TestDefaultDBPath(t *testing.T) {
	for _, c := range []struct {
		euid int
		env  map[string]string
		want string
	}{
		{0, map[string]string{"HOME": "/root", "XDG_STATE_HOME": "/x"}, "/var/lib/llamadash/llamadash.db"},
		{1000, map[string]string{"HOME": "/home/u", "XDG_STATE_HOME": "/st"}, filepath.Join("/st", "llamadash", "llamadash.db")},
		{1000, map[string]string{"HOME": "/home/u"}, filepath.Join("/home/u", ".local", "state", "llamadash", "llamadash.db")},
		{1000, map[string]string{"HOME": "/home/u", "XDG_STATE_HOME": "relative"}, filepath.Join("/home/u", ".local", "state", "llamadash", "llamadash.db")},
	} {
		got, err := defaultDBPath(c.euid, envFn(c.env))
		if err != nil || got != c.want {
			t.Errorf("euid %d env %v: %q %v, want %q", c.euid, c.env, got, err, c.want)
		}
	}
	if p, err := defaultDBPath(1000, envFn(nil)); err == nil {
		t.Errorf("no home: %q, want an error", p)
	}
}

// orderRec is a Recorder and pruner that records the order of its blocking calls.
type orderRec struct {
	mu     sync.Mutex
	calls  []string
	pruned chan [2]int
	fail   []error // Prune results, in order; nil when exhausted
}

func (r *orderRec) note(s string) { r.mu.Lock(); r.calls = append(r.calls, s); r.mu.Unlock() }
func (r *orderRec) SetTariff(time.Time, float64, string) error {
	time.Sleep(20 * time.Millisecond) // a slow writer queue: the prune must still come after
	r.note("tariff")
	return nil
}
func (r *orderRec) AddSample(store.Sample)          {}
func (r *orderRec) AddRequest(stats.Finished)       {}
func (r *orderRec) AddUsage(time.Time, store.Usage) {}
func (r *orderRec) Err() error                      { return nil }
func (r *orderRec) RecentRequests(int) ([]stats.Finished, error) {
	r.note("recent")
	return nil, nil
}
func (r *orderRec) Totals(time.Time) (a, b, c store.Totals, err error) {
	r.note("totals")
	return
}
func (r *orderRec) Prune(_ time.Time, sampleDays, requestDays int) error {
	r.note("prune")
	r.mu.Lock()
	var err error
	if len(r.fail) > 0 {
		err, r.fail = r.fail[0], r.fail[1:]
	}
	r.mu.Unlock()
	r.pruned <- [2]int{sampleDays, requestDays}
	return err
}

func TestStartBeforeFirstPrune(t *testing.T) {
	rec := &orderRec{pruned: make(chan [2]int, 1)}
	c := collect.New(collect.Config{URL: "http://127.0.0.1:1", GPU: collect.GPUOff, Store: rec, KWhPrice: 0.3})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDB(ctx, c, rec, 30, func(error) {})
	if days := <-rec.pruned; days != [2]int{30, requestDays} {
		t.Fatalf("prune days %v, want samples 30, requests %d", days, requestDays)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := strings.Join(rec.calls, ","); got != "recent,totals,tariff,prune" {
		t.Fatalf("call order %s", got)
	}
}

func TestPruneRetentionAndRetry(t *testing.T) {
	if requestDays != 90 {
		t.Fatalf("requests are kept %d days, want 90", requestDays)
	}
	busy := errors.New("busy")
	rec := &orderRec{pruned: make(chan [2]int, 1), fail: []error{busy, busy}}
	var waits []time.Duration
	tick := make(chan time.Time)
	wait := func(d time.Duration) <-chan time.Time { waits = append(waits, d); return tick }
	reports := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pruneLoop(ctx, rec, 7, func(err error) { reports <- err }, wait); close(done) }()
	for i, want := range []error{busy, busy, nil, nil} {
		if days := <-rec.pruned; days != [2]int{7, 90} {
			t.Fatalf("prune %d days %v: request retention must not follow --retain-days", i, days)
		}
		if got := <-reports; got != want {
			t.Fatalf("prune %d reported %v, want %v", i, got, want)
		}
		if i < 3 {
			tick <- time.Time{}
		}
	}
	cancel()
	<-done
	wantWaits := []time.Duration{pruneRetry, pruneRetry, pruneEvery, pruneEvery}
	if fmt.Sprint(waits) != fmt.Sprint(wantWaits) {
		t.Fatalf("waits %v, want %v", waits, wantWaits)
	}
}
