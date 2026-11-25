// Package store keeps the dashboard's history in a SQLite database (pure Go driver):
// periodic samples, finished requests, per-day usage totals and the energy tariff.
//
// All writes go through one goroutine that owns the write connection. Add* only queue
// in memory and never block; the queue is written in one transaction every 10 s or
// once 256 items are waiting. Reads use a separate read-only pool and see what is on
// disk. The file is in WAL mode with synchronous=NORMAL: a crash or power loss loses
// at most the batches not yet flushed (or the last committed one), never the file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"llamadash/internal/stats"
)

const (
	queueSamples  = 1024             // queued samples; the oldest is dropped beyond this
	queueRequests = 256              // queued requests; the oldest is dropped only beyond this
	flushAt       = 256              // queued items that trigger a flush before the timer
	flushEvery    = 10 * time.Second // flush timer
	busyTimeout   = 2 * time.Second  // SQLite busy_timeout
	maxFails      = 6                // consecutive failed flushes before pausing
	pauseFor      = 5 * time.Minute  // pause after maxFails failures
	closeTimeout  = 5 * time.Second  // bound on Close's final flush
)

// ErrClosed is returned by operations on a closed Store.
var ErrClosed = errors.New("store: closed")

// Sample is one periodic measurement. A float that is NaN (or not finite) has no
// reading and is stored as NULL; so are the memory figures without their Has flag.
type Sample struct {
	At                           time.Time
	GenRate, PromptRate          float64
	Processing, Queued           int
	GPUPowerW, CPUPowerW         float64
	GPUUtil, GPUMemPct, GPUTempC float64
	HostCPUPct                   float64
	HostMemUsedKB                uint64
	HasHostMem                   bool
	ProcCPUPct                   float64
	ProcRSSKB                    uint64
	HasProcRSS                   bool
}

// Usage holds deltas to add to a day.
type Usage struct {
	GenTokens, PromptTokens, Requests int64
	GPUEnergyWh, CPUEnergyWh, Cost    float64
}

func (u Usage) add(v Usage) Usage {
	return Usage{
		GenTokens: u.GenTokens + v.GenTokens, PromptTokens: u.PromptTokens + v.PromptTokens,
		Requests: u.Requests + v.Requests, GPUEnergyWh: u.GPUEnergyWh + v.GPUEnergyWh,
		CPUEnergyWh: u.CPUEnergyWh + v.CPUEnergyWh, Cost: u.Cost + v.Cost,
	}
}

// Totals is a sum of daily rows; HasData is false when no row matched.
type Totals struct {
	Usage
	HasData bool
}

// Point is one Series value.
type Point struct {
	At time.Time
	V  float64
}

// config holds the knobs tests replace.
type config struct {
	tick <-chan time.Time // flush timer; nil = a real 10 s ticker
	busy time.Duration    // busy_timeout; 0 = busyTimeout
	now  func() time.Time // clock for the retry pause only (never for data); nil = time.Now
}

// Store is the history database. Its methods are safe for concurrent use.
type Store struct {
	path string
	cfg  config
	wdb  *sql.DB
	conn *sql.Conn // the write connection; used only by the writer goroutine
	rdb  *sql.DB   // read-only pool

	mu         sync.Mutex
	q          batch     // queued writes
	maxSamples int       // high-water mark of queued samples
	err        error     // last write error
	fails      int       // consecutive failed flushes
	pauseUntil time.Time // no timer/size flushes before this (after maxFails failures)
	attempts   int       // flush attempts (tests)

	wake     chan struct{} // queue reached flushAt
	ops      chan op
	stop     chan struct{}
	done     chan struct{}
	finalErr error // final flush result; read after done is closed

	closeOnce sync.Once
	closeErr  error
}

// op is a function run on the writer goroutine with the write connection.
type op struct {
	fn  func(ctx context.Context, c *sql.Conn) error
	res chan error
}

// Open opens or creates the database at path: it creates the parent directory (0700)
// and the file (0600) if missing, switches it to WAL and migrates the schema. A file that
// is not a SQLite database, or one with a newer schema, is rejected and left untouched.
func Open(path string) (*Store, error) { return open(path, config{}) }

func open(path string, cfg config) (*Store, error) {
	if path == "" || strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("store: invalid database path %q", path)
	}
	if cfg.busy <= 0 {
		cfg.busy = busyTimeout
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := prepareFile(path); err != nil {
		return nil, err
	}
	ctx := context.Background()
	busy := fmt.Sprintf("_pragma=busy_timeout(%d)", cfg.busy.Milliseconds())

	wdb, err := sql.Open("sqlite", path+"?"+busy+"&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	conn, err := wdb.Conn(ctx)
	if err != nil {
		wdb.Close()
		return nil, atOpen(fmt.Errorf("store: open %s: %w", path, err))
	}
	fail := func(err error) (*Store, error) {
		conn.Close()
		wdb.Close()
		return nil, atOpen(err)
	}
	v, err := checkVersion(ctx, conn, path)
	if err != nil {
		return fail(err)
	}
	if err := migrate(ctx, conn, v); err != nil {
		return fail(err)
	}
	rdb, err := sql.Open("sqlite", path+"?"+busy+"&_pragma=query_only(1)")
	if err != nil {
		return fail(fmt.Errorf("store: %w", err))
	}
	rdb.SetMaxOpenConns(2)
	if err := rdb.PingContext(ctx); err != nil {
		rdb.Close()
		return fail(fmt.Errorf("store: open reader: %w", err))
	}

	s := &Store{
		path: path, cfg: cfg, wdb: wdb, conn: conn, rdb: rdb,
		q:    batch{usage: map[string]Usage{}},
		wake: make(chan struct{}, 1), ops: make(chan op),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	tick := cfg.tick
	var ticker *time.Ticker
	if tick == nil {
		ticker = time.NewTicker(flushEvery)
		tick = ticker.C
	}
	go func() {
		if ticker != nil {
			defer ticker.Stop()
		}
		s.run(tick)
	}()
	return s, nil
}

var sqliteMagic = []byte("SQLite format 3\x00")

// prepareFile creates path (0600) when missing, and rejects an existing non-empty file
// that does not start with the SQLite header, without changing it.
func prepareFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return f.Close()
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("store: %w", err)
	}
	f, err = os.Open(path)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, head)
	if n == 0 && errors.Is(err, io.EOF) {
		return nil // empty file: SQLite treats it as a new database
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("store: read %s: %w", path, err)
	}
	if err != nil || string(head) != string(sqliteMagic) {
		return fmt.Errorf("store: %s: %w", path, ErrNotDatabase)
	}
	return nil
}

// Close flushes the queue (bounded to 5 s) and closes the database. It is safe to call
// more than once; later calls return the first call's result.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
			s.closeErr = errors.Join(s.finalErr, s.rdb.Close(), s.conn.Close(), s.wdb.Close())
		case <-time.After(closeTimeout + time.Second):
			// The writer is stuck in a statement; closing its connection now would wait
			// for it, so let that happen in the background.
			s.closeErr = errors.Join(errors.New("store: final flush did not finish"), s.rdb.Close())
			go func() {
				<-s.done
				s.conn.Close()
				s.wdb.Close()
			}()
		}
	})
	return s.closeErr
}

// Err returns the last write error, nil when the last flush succeeded.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Flush writes the queue now, even while retries are paused, and returns the result.
func (s *Store) Flush() error {
	return s.do(func(ctx context.Context, _ *sql.Conn) error { return s.flush(ctx) })
}

// do runs fn on the writer goroutine and waits for it.
func (s *Store) do(fn func(ctx context.Context, c *sql.Conn) error) error {
	o := op{fn: fn, res: make(chan error, 1)}
	select {
	case s.ops <- o:
		return <-o.res
	case <-s.stop:
		return ErrClosed
	}
}

// AddSample queues x. It never blocks; beyond 1024 queued samples the oldest is dropped.
func (s *Store) AddSample(x Sample) {
	s.mu.Lock()
	s.q.samples = pushBounded(s.q.samples, x, queueSamples)
	s.maxSamples = max(s.maxSamples, len(s.q.samples))
	s.notifyLocked()
	s.mu.Unlock()
}

// AddRequest queues f. It never blocks; beyond 256 queued requests the oldest is dropped.
func (s *Store) AddRequest(f stats.Finished) {
	s.mu.Lock()
	s.q.requests = pushBounded(s.q.requests, f, queueRequests)
	s.notifyLocked()
	s.mu.Unlock()
}

// AddUsage adds u to the pending total of at's local date. It never blocks and never drops.
func (s *Store) AddUsage(at time.Time, u Usage) {
	day := dayOf(at)
	u = u.sanitised()
	s.mu.Lock()
	s.q.usage[day] = s.q.usage[day].add(u)
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *Store) notifyLocked() {
	if s.q.len() >= flushAt {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// dayOf is the local date of t as YYYY-MM-DD.
func dayOf(t time.Time) string { return t.Local().Format("2006-01-02") }

// pushBounded appends v, dropping from the front to keep at most n elements.
func pushBounded[T any](s []T, v T, n int) []T {
	if len(s) >= n {
		s = s[len(s)-n+1:]
	}
	return append(s, v)
}
