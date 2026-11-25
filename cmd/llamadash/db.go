package main

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/stats"
	"llamadash/internal/store"
	"llamadash/internal/ui"
)

const (
	startWait   = 2 * time.Second // bound on the start-up database reads
	pruneEvery  = 24 * time.Hour
	pruneRetry  = 10 * time.Minute // after a failed prune
	requestDays = 90               // finished requests are kept this long, whatever --retain-days says
)

// lockRetryEvery is how often a read-only dashboard tries the lock again.
const lockRetryEvery = 30 * time.Second

// retryAfter times the lock retries (tests replace it).
var retryAfter = after

// geteuid is replaced in tests so they never use the root default path.
var geteuid = os.Geteuid

// defaultDBPath is /var/lib/llamadash/llamadash.db for root, else
// $XDG_STATE_HOME/llamadash/llamadash.db (an absolute XDG_STATE_HOME only), else
// ~/.local/state/llamadash/llamadash.db.
func defaultDBPath(euid int, env func(string) string) (string, error) {
	if euid == 0 {
		return "/var/lib/llamadash/llamadash.db", nil
	}
	if x := env("XDG_STATE_HOME"); x != "" && path.IsAbs(filepath.ToSlash(x)) {
		return filepath.Join(x, "llamadash", "llamadash.db"), nil
	}
	if home := env("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "llamadash", "llamadash.db"), nil
	}
	return "", errors.New("no home directory for the default database path")
}

// lockDB takes the database's lock file (tests replace it).
var lockDB = store.Lock

// startSession opens the database and creates the collector. oneShot (--once,
// --frame) records nothing and never takes the lock; the live dashboard records
// unless another llamadash holds the database's lock, in which case it only reads
// and tries the lock again every lockRetryEvery, taking over recording once it gets
// it. finish stops that, flushes the last window when recording and closes the
// store and the lock; call it once, after the last Poll.
func startSession(o options, env func(string) string, oneShot bool) (c *collect.Collector, src ui.SeriesSource, finish func()) {
	cfg := collect.Config{URL: o.url, APIKey: o.apiKey, Interval: o.interval, GPU: o.gpu,
		KWhPrice: o.kwhPrice, Currency: o.currency, OneShot: oneShot, PSUWatts: o.psuWatts,
		KnownFans: o.knownFans, OnNewFans: o.onNewFans}
	st, lock := openDB(o, env, oneShot, &cfg)
	c = collect.New(cfg)
	recording := st != nil && !oneShot && !cfg.DBReadOnly
	ctx, cancel := context.WithCancel(context.Background())
	var retry sync.WaitGroup
	if recording {
		startDB(ctx, c, st, o.retainDays, c.SetPruneErr)
	} else {
		c.Start(time.Now(), startWait) // read-only: seeds Recent and totals, writes nothing
	}
	if ro, ok := cfg.Store.(*readOnlyStore); ok && cfg.DBReadOnly {
		retry.Add(1)
		go func() {
			defer retry.Done()
			l := retryLock(ctx, ro.path, retryAfter)
			if l == nil {
				return
			}
			// the window from the read-only period is dropped, then writes pass
			c.StartRecording(func() { ro.writing.Store(true) })
			if o.kwhPrice > 0 {
				st.SetTariff(time.Now(), o.kwhPrice, o.currency)
			}
			go pruneLoop(ctx, st, o.retainDays, c.SetPruneErr, after)
			lock, recording = l, true // read by finish only after retry.Wait
		}()
	}
	if st != nil {
		src = st // the store itself: reads only, so a read-only run stays read-only
	}
	return c, src, func() {
		cancel()
		retry.Wait() // a takeover either finished or never happens
		if recording {
			c.FlushHistory(time.Now()) // the last partial window, before the store closes
		}
		if st != nil {
			st.Close()
		}
		lock.Close()
	}
}

// retryLock tries the database's lock every lockRetryEvery until it gets it or ctx
// ends; it returns the lock, or nil when ctx ended first.
func retryLock(ctx context.Context, dbPath string, wait func(time.Duration) <-chan time.Time) *store.FileLock {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wait(lockRetryEvery):
		}
		if l, err := lockDB(dbPath); err == nil {
			if ctx.Err() != nil { // quitting meanwhile: never write after finish
				l.Close()
				return nil
			}
			return l
		}
	}
}

// openDB opens the history database named by the options into cfg. A failure is
// not fatal: it goes to cfg.DBOpenErr and the dashboard runs without history. With
// readOnly the collector gets a view that never writes. Otherwise it takes the
// database's lock: when another process holds it, the collector gets the read-only
// view and cfg.DBReadOnly is set. It returns a nil store when there is no database.
func openDB(o options, env func(string) string, readOnly bool, cfg *collect.Config) (*store.Store, *store.FileLock) {
	if o.noDB {
		return nil, nil
	}
	p := o.db
	if p == "" {
		var err error
		if p, err = defaultDBPath(geteuid(), env); err != nil {
			cfg.DBOpenErr = err
			return nil, nil
		}
	}
	st, err := store.Open(p)
	if err != nil {
		cfg.DBOpenErr = err
		return nil, nil
	}
	cfg.Store = st
	if readOnly {
		cfg.Store = &readOnlyStore{Store: st}
		return st, nil
	}
	lock, err := lockDB(p)
	switch {
	case errors.Is(err, store.ErrLocked):
		cfg.Store, cfg.DBReadOnly = &readOnlyStore{Store: st, path: p}, true
	case err != nil: // the lock file cannot be created: as if the database did not open
		st.Close()
		cfg.Store, cfg.DBOpenErr = nil, err
		return nil, nil
	}
	return st, lock
}

// readOnlyStore is the store for --once, --frame and a dashboard without the lock:
// reads work, writes are dropped until writing is set (the lock was taken over).
type readOnlyStore struct {
	*store.Store
	path    string // the database, for retrying the lock
	writing atomic.Bool
}

func (s *readOnlyStore) SetTariff(at time.Time, price float64, currency string) error {
	if s.writing.Load() {
		return s.Store.SetTariff(at, price, currency)
	}
	return nil
}

func (s *readOnlyStore) AddSample(x store.Sample) {
	if s.writing.Load() {
		s.Store.AddSample(x)
	}
}

func (s *readOnlyStore) AddRequest(f stats.Finished) {
	if s.writing.Load() {
		s.Store.AddRequest(f)
	}
}

func (s *readOnlyStore) AddUsage(at time.Time, u store.Usage) {
	if s.writing.Load() {
		s.Store.AddUsage(at, u)
	}
}

// pruner is the part of the store pruneLoop uses.
type pruner interface {
	Prune(now time.Time, sampleDays, requestDays int) error
}

// starter is the part of the collector startDB uses.
type starter interface {
	Start(now time.Time, wait time.Duration) bool
}

// startDB runs the collector's start-up reads (bounded), then starts the prune loop on
// its own goroutine. The order matters: both the tariff write and a prune go through
// the store's single writer, and a slow first prune must not hold up the tariff.
func startDB(ctx context.Context, c starter, p pruner, sampleDays int, report func(error)) {
	c.Start(time.Now(), startWait) // on timeout the reads finish in the background
	go pruneLoop(ctx, p, sampleDays, report, after)
}

func after(d time.Duration) <-chan time.Time { return time.After(d) }

// pruneLoop prunes now and then every 24 h (10 min after a failure) until ctx ends,
// one prune at a time. Samples are kept sampleDays, requests requestDays. Each result
// goes to report, so a failing prune shows in the status line.
func pruneLoop(ctx context.Context, p pruner, sampleDays int, report func(error), wait func(time.Duration) <-chan time.Time) {
	for {
		err := p.Prune(time.Now(), sampleDays, requestDays)
		report(err)
		next := pruneEvery
		if err != nil {
			next = pruneRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-wait(next):
		}
	}
}
