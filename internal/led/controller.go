package led

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// Config is how a Controller drives the lights.
type Config struct {
	Run        Runner                    // nil: the controller does nothing
	Device     string                    // "" = every motherboard
	ConfigDir  string                    // openrgb's --config directory; created 0700 when missing
	Lock       func() (io.Closer, error) // nil: always the owner. An error means another copy owns the lights.
	Now        func() time.Time          // nil: time.Now
	Hold       int                       // polls a state must last before it is shown; 0 -> 3
	Timeout    time.Duration             // of one openrgb run; 0 -> 20s
	RetryEvery time.Duration             // after a failure; 0 -> 60s
	LockEvery  time.Duration             // between attempts to take the lock; 0 -> 30s
	ExitWait   time.Duration             // the most Close waits; 0 -> 3s

	afterApply func() // tests: called by the worker after each pass
}

const (
	noteUnavailable = "led: unavailable"
	keepLogs        = 5  // openrgb log files kept in ConfigDir/logs
	maxHold         = 60 // a larger Hold would only make the lights late
)

var errNoDevice = errors.New("led: no matching device")

// Controller shows the dashboard's status on the lights. Offer is called once
// per poll and never waits: a single worker goroutine runs openrgb, one run at
// a time, and always for the newest state.
type Controller struct {
	cfg   Config
	inert bool

	mu       sync.Mutex
	closed   bool
	owner    bool // this copy holds the lock
	lock     io.Closer
	lockAt   time.Time // the last attempt to take the lock
	recent   []State   // the last Hold offers, oldest first
	held     State     // the state to show
	haveHeld bool
	sent     State // the last state handed to the worker
	haveSent bool
	pending  State // waiting for the worker
	havePend bool
	failed   bool // the last run failed
	failedAt time.Time
	deadline time.Time // set by Close: when the switch-off must be over

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	base context.Context // ends when Close is called
	end  context.CancelFunc
	once sync.Once

	// the worker's own
	devs    []Device // nil: read the device list first
	touched bool     // a colour run was started, so the lights may be ours
}

// New starts a controller. It takes the lock at once when it is free.
func New(cfg Config) *Controller {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.Timeout, 20*time.Second)
	def(&cfg.RetryEvery, 60*time.Second)
	def(&cfg.LockEvery, 30*time.Second)
	def(&cfg.ExitWait, 3*time.Second)
	if cfg.Hold <= 0 {
		cfg.Hold = 3
	}
	cfg.Hold = min(cfg.Hold, maxHold)
	c := &Controller{cfg: cfg, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	c.base, c.end = context.WithCancel(context.Background())
	if cfg.Run == nil {
		c.inert = true
		close(c.done)
		return c
	}
	c.tryLock(cfg.Now())
	go c.work()
	return c
}

// tryLock takes the lock if it is free. The caller holds mu, or is New.
func (c *Controller) tryLock(now time.Time) {
	c.lockAt = now
	if c.cfg.Lock == nil {
		c.owner = true
		return
	}
	if l, err := c.cfg.Lock(); err == nil {
		c.owner, c.lock = true, l
	}
}

// Offer reports the state of one poll. It never blocks.
//
// The state shown changes when the last Hold polls agree on a new one. It also
// moves when all of them are on one side of it: up to the mildest of them, or
// down to the worst. So a reading that hovers between amber and red cannot
// leave green showing, and one odd poll changes nothing.
func (c *Controller) Offer(st State) {
	if c == nil || c.inert {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.recent = append(c.recent, st); len(c.recent) > c.cfg.Hold {
		c.recent = slices.Delete(c.recent, 0, len(c.recent)-c.cfg.Hold)
	}
	if len(c.recent) == c.cfg.Hold {
		lo, hi := slices.Min(c.recent), slices.Max(c.recent)
		switch {
		case !c.haveHeld && lo == hi, c.haveHeld && lo > c.held:
			c.held, c.haveHeld = lo, true
		case c.haveHeld && hi < c.held:
			c.held = hi
		}
	}
	now := c.cfg.Now()
	if !c.owner {
		if now.Sub(c.lockAt) < c.cfg.LockEvery {
			return
		}
		if c.tryLock(now); !c.owner {
			return
		}
	}
	if !c.haveHeld {
		return
	}
	switch {
	case c.failed: // leave the lights alone, and try again later whatever the state
		if now.Sub(c.failedAt) < c.cfg.RetryEvery {
			return
		}
		c.failedAt = now
	case c.haveSent && c.sent == c.held:
		return
	}
	c.sent, c.haveSent = c.held, true
	c.pending, c.havePend = c.held, true
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Note is "" while the lights work or belong to another copy, else "led: unavailable".
func (c *Controller) Note() string {
	if c == nil || c.inert {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner && c.failed {
		return noteUnavailable
	}
	return ""
}

// Close switches the lights off if this copy may have changed them, waits at
// most ExitWait for that, stops the worker and gives up the lock. A run in
// flight is ended first. Close may be called more than once.
func (c *Controller) Close() {
	if c == nil || c.inert {
		return
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.closed, c.havePend = true, false
		c.deadline = time.Now().Add(c.cfg.ExitWait)
		c.mu.Unlock()
		c.end()
		close(c.stop)
		t := time.NewTimer(c.cfg.ExitWait)
		defer t.Stop()
		select {
		case <-c.done:
		case <-t.C: // a runner that ignores its context: do not hold the exit up
		}
		c.mu.Lock()
		if c.lock != nil {
			c.lock.Close()
			c.lock = nil
		}
		c.owner = false
		c.mu.Unlock()
	})
}

// work is the single goroutine that runs openrgb.
func (c *Controller) work() {
	defer close(c.done)
	for {
		select {
		case <-c.stop:
			c.switchOff()
			return
		case <-c.wake:
		}
		for c.base.Err() == nil {
			c.mu.Lock()
			st, ok := c.pending, c.havePend
			c.havePend = false
			c.mu.Unlock()
			if !ok {
				break
			}
			ctx, cancel := context.WithTimeout(c.base, c.cfg.Timeout)
			err := c.apply(ctx, st)
			cancel()
			c.mu.Lock()
			if c.failed = err != nil; c.failed {
				c.failedAt = c.cfg.Now()
			}
			c.mu.Unlock()
			if err != nil && c.base.Err() == nil {
				c.devs = nil // the device may have gone or been renumbered: list again
			}
			if c.cfg.afterApply != nil {
				c.cfg.afterApply()
			}
		}
	}
}

// switchOff is the worker's last act. A run that was started may have changed
// the lights even if it failed or was cut short, so it counts.
func (c *Controller) switchOff() {
	if !c.touched {
		return
	}
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_ = c.apply(ctx, Off)
}

// apply puts the lights in st: it reads the device list when it is not known,
// then runs openrgb once for all devices.
func (c *Controller) apply(ctx context.Context, st State) error {
	dir := c.cfg.ConfigDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer func() { _ = PruneLogs(filepath.Join(dir, "logs"), keepLogs) }()
	if c.devs == nil {
		out, err := c.cfg.Run(ctx, ListArgs(dir)...)
		if err != nil {
			return err
		}
		devs := Pick(ParseDevices(out), c.cfg.Device)
		if len(devs) == 0 {
			return errNoDevice
		}
		c.devs = devs
	}
	c.touched = true
	_, err := c.cfg.Run(ctx, Args(c.devs, st, dir)...)
	return err
}
