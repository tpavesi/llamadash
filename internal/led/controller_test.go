package led

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// fake is a scripted openrgb: it records every argument list.
type fake struct {
	mu      sync.Mutex
	calls   [][]string
	list    []byte
	listErr error         // returned by a list run
	runErr  error         // returned by a colour or off run
	block   chan struct{} // when set, a colour run waits for it (or for its context)
	hangAll bool          // every run waits for its context
	hangRun bool          // colour and off runs wait for their context
	started chan []string
}

func newFake(t *testing.T) *fake {
	return &fake{list: read(t, "list_detailed.txt"), started: make(chan []string, 256)}
}

func isList(args []string) bool { return slices.Contains(args, "--list-detailed") }

func (f *fake) run(ctx context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	list, listErr, runErr, block := f.list, f.listErr, f.runErr, f.block
	hang := f.hangAll || (f.hangRun && !isList(args))
	f.mu.Unlock()
	f.started <- args
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if isList(args) {
		return list, listErr
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, runErr
}

func (f *fake) set(edit func(*fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f)
}

func (f *fake) all() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// colours are the -c values of the colour runs so far, "off" for an Off run.
func (f *fake) colours() (out []string) {
	for _, c := range f.all() {
		switch {
		case isList(c):
		case slices.Contains(c, "-c"):
			out = append(out, c[len(c)-1])
		default:
			out = append(out, "off")
		}
	}
	return out
}

func (f *fake) lists() (n int) {
	for _, c := range f.all() {
		if isList(c) {
			n++
		}
	}
	return n
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t       *testing.T
	c       *Controller
	f       *fake
	clk     *clock
	applied chan struct{}
}

func start(t *testing.T, f *fake, edit func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, f: f, clk: &clock{t: time.Unix(1_700_000_000, 0)}, applied: make(chan struct{}, 256)}
	cfg := Config{Run: f.run, ConfigDir: t.TempDir(), Now: h.clk.now, ExitWait: 2 * time.Second,
		afterApply: func() { h.applied <- struct{}{} }}
	if edit != nil {
		edit(&cfg)
	}
	h.c = New(cfg)
	t.Cleanup(h.c.Close)
	return h
}

// wait is for one worker pass to finish.
func (h *harness) wait() {
	h.t.Helper()
	select {
	case <-h.applied:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the worker did not finish a run")
	}
}

func (h *harness) offer(st State, n int) {
	for range n {
		h.c.Offer(st)
	}
}

// green applies green and waits for it.
func (h *harness) green() {
	h.t.Helper()
	h.offer(Green, 3)
	h.wait()
	if got := h.f.colours(); !slices.Equal(got, []string{"00FF00"}) {
		h.t.Fatalf("colours = %q, want green", got)
	}
}

func (h *harness) wantColours(want ...string) {
	h.t.Helper()
	if got := h.f.colours(); !slices.Equal(got, want) {
		h.t.Errorf("colour runs = %q, want %q", got, want)
	}
}

func TestHoldsThreePolls(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.offer(Green, 2)
	if n := len(h.f.all()); n != 0 {
		t.Fatalf("%d runs after two polls", n)
	}
	h.c.Offer(Green)
	h.wait()
	calls := h.f.all()
	if len(calls) != 2 || !isList(calls[0]) {
		t.Fatalf("runs = %q, want a list then a colour", calls)
	}
	devs := ParseDevices(h.f.list)
	if want := Args(devs, Green, calls[1][2]); !slices.Equal(calls[1], want) {
		t.Errorf("colour run = %q, want %q", calls[1], want)
	}
	if want := ListArgs(calls[1][2]); !slices.Equal(calls[0], want) {
		t.Errorf("list run = %q, want %q", calls[0], want)
	}
	if h.c.Note() != "" {
		t.Errorf("note = %q", h.c.Note())
	}
}

func TestFlappingNeverRuns(t *testing.T) {
	h := start(t, newFake(t), nil)
	for i := range 20 {
		h.c.Offer([]State{Green, Amber}[i%2])
	}
	h.offer(Red, 2) // two in a row is still not three
	h.c.Close()
	if calls := h.f.all(); len(calls) != 0 {
		t.Errorf("runs = %q, want none", calls)
	}
}

func TestChangeAfterHold(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.green()
	h.offer(Amber, 2)
	h.c.Offer(Green) // a blip: the count starts again
	h.offer(Amber, 3)
	h.wait()
	h.wantColours("00FF00", "FF7E00")
	if n := h.f.lists(); n != 1 {
		t.Errorf("%d list runs, want 1", n)
	}
}

func TestSameStateNotRepeated(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.green()
	h.offer(Green, 10)
	h.c.Close()
	h.wantColours("00FF00", "off")
}

func TestNewestWins(t *testing.T) {
	f := newFake(t)
	f.block = make(chan struct{})
	h := start(t, f, nil)
	h.offer(Green, 3)
	for args := range f.started { // until the green run is in flight
		if !isList(args) {
			break
		}
	}
	h.offer(Amber, 3)
	h.offer(Red, 3)
	close(f.block)
	h.wait()
	h.wait()
	h.wantColours("00FF00", "FF0000")
}

func TestFailureThenRetry(t *testing.T) {
	f := newFake(t)
	f.listErr = ErrUnavailable
	h := start(t, f, nil)
	h.offer(Green, 3)
	h.wait()
	if h.c.Note() != "led: unavailable" {
		t.Fatalf("note = %q", h.c.Note())
	}
	h.wantColours()
	h.clk.add(59 * time.Second)
	h.c.Offer(Green)
	h.c.Offer(Amber) // a change does not retry early either
	h.offer(Green, 3)
	if n := len(f.all()); n != 1 {
		t.Fatalf("%d runs before the retry time, want 1", n)
	}
	f.set(func(f *fake) { f.listErr = nil })
	h.clk.add(time.Second)
	h.c.Offer(Green)
	h.wait()
	h.wantColours("00FF00")
	if n := f.lists(); n != 2 {
		t.Errorf("%d list runs, want 2", n)
	}
	if h.c.Note() != "" {
		t.Errorf("note after a good run = %q", h.c.Note())
	}
	h.offer(Green, 5) // and no more runs for the same state
	h.c.Close()
	h.wantColours("00FF00", "off")
}

func TestRunFailureListsAgain(t *testing.T) {
	f := newFake(t)
	h := start(t, f, nil)
	h.green()
	f.set(func(f *fake) { f.runErr = errors.New("exit status 1") })
	h.offer(Amber, 3)
	h.wait()
	if h.c.Note() != "led: unavailable" {
		t.Fatalf("note = %q", h.c.Note())
	}
	f.set(func(f *fake) { f.runErr = nil })
	h.clk.add(time.Minute)
	h.c.Offer(Red) // the retry sends the state that held, not this one poll
	h.wait()
	h.wantColours("00FF00", "FF7E00", "FF7E00")
	if n := f.lists(); n != 2 {
		t.Errorf("%d list runs, want 2 (the devices are read again after a failure)", n)
	}
}

func TestNoMatchingDevice(t *testing.T) {
	for name, edit := range map[string]func(*fake, *Config){
		"empty list":   func(f *fake, _ *Config) { f.list = read(t, "list_empty.txt") },
		"garbage":      func(f *fake, _ *Config) { f.list = []byte("Segmentation fault\n") },
		"wrong name":   func(_ *fake, c *Config) { c.Device = "no such device" },
		"not a board":  func(f *fake, _ *Config) { f.list = []byte("0: Example Keyboard\n  Type: Keyboard\n  Modes: Static\n") },
		"missing prog": func(f *fake, _ *Config) { f.listErr = ErrUnavailable },
	} {
		f := newFake(t)
		h := start(t, f, func(c *Config) { edit(f, c) })
		h.offer(Red, 3)
		h.wait()
		if h.c.Note() != "led: unavailable" {
			t.Errorf("%s: note = %q", name, h.c.Note())
		}
		h.c.Close()
		if got := f.colours(); len(got) != 0 {
			t.Errorf("%s: colour runs = %q, want none", name, got)
		}
	}
}

func TestDeviceByName(t *testing.T) {
	f := newFake(t)
	f.list = read(t, "list_two_devices.txt")
	h := start(t, f, func(c *Config) { c.Device = "keyboard" })
	h.offer(Red, 3)
	h.wait()
	calls := f.all()
	if got := calls[len(calls)-1]; !slices.Equal(got[3:], []string{"-d", "1", "-m", "Static", "-c", "FF0000"}) {
		t.Errorf("run = %q", got)
	}
}

func TestHungRunTimesOut(t *testing.T) {
	f := newFake(t)
	f.hangAll = true
	h := start(t, f, func(c *Config) { c.Timeout = 20 * time.Millisecond })
	began := time.Now()
	h.offer(Green, 3)
	h.offer(Amber, 3)
	if d := time.Since(began); d > time.Second {
		t.Errorf("six offers took %v", d)
	}
	h.wait()
	if h.c.Note() != "led: unavailable" {
		t.Errorf("note = %q", h.c.Note())
	}
	began = time.Now()
	h.c.Close()
	if d := time.Since(began); d > time.Second {
		t.Errorf("Close took %v", d)
	}
}

// A colour run that hangs may still have changed the lights: Close tries to
// switch them off, and gives up after ExitWait.
func TestCloseBoundedWhenHung(t *testing.T) {
	f := newFake(t)
	f.hangRun = true
	h := start(t, f, func(c *Config) { c.ExitWait = 30 * time.Millisecond })
	h.offer(Green, 3)
	for args := range f.started {
		if !isList(args) {
			break
		}
	}
	began := time.Now()
	h.c.Close()
	if d := time.Since(began); d > time.Second {
		t.Errorf("Close took %v", d)
	}
	h.wantColours("00FF00", "off")
	select {
	case <-h.c.done:
	case <-time.After(2 * time.Second):
		t.Error("the worker is still running")
	}
}

type closer struct{ closed chan struct{} }

func (c closer) Close() error { close(c.closed); return nil }

func TestNoLockNeverRuns(t *testing.T) {
	f := newFake(t)
	h := start(t, f, func(c *Config) { c.Lock = func() (io.Closer, error) { return nil, errors.New("held") } })
	h.offer(Red, 3)
	h.clk.add(time.Hour)
	h.offer(Red, 3)
	if h.c.Note() != "" {
		t.Errorf("note = %q", h.c.Note())
	}
	h.c.Close()
	if calls := f.all(); len(calls) != 0 {
		t.Errorf("runs = %q, want none", calls)
	}
}

func TestLockTakenLater(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	free, tries := false, 0
	lk := closer{make(chan struct{})}
	h := start(t, f, func(c *Config) {
		c.Lock = func() (io.Closer, error) {
			mu.Lock()
			defer mu.Unlock()
			if tries++; !free {
				return nil, errors.New("held")
			}
			return lk, nil
		}
	})
	mu.Lock()
	free = true
	mu.Unlock()
	h.clk.add(29 * time.Second)
	h.offer(Red, 3)
	mu.Lock()
	if tries != 1 {
		t.Errorf("%d lock attempts before 30 s, want 1", tries)
	}
	mu.Unlock()
	h.clk.add(time.Second)
	h.c.Offer(Red) // takes the lock; red has already held for three polls
	h.wait()
	h.wantColours("FF0000")
	h.c.Close()
	h.wantColours("FF0000", "off")
	select {
	case <-lk.closed:
	default:
		t.Error("Close did not release the lock")
	}
}

func TestCloseSwitchesOff(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.green()
	h.c.Close()
	calls := h.f.all()
	if last := calls[len(calls)-1]; !slices.Equal(last[len(last)-2:], []string{"-m", "Off"}) {
		t.Errorf("last run = %q, want it to end -m Off", last)
	}
	h.c.Close()
	h.c.Offer(Red)
	h.offer(Red, 5)
	if n := len(h.f.all()); n != len(calls) {
		t.Errorf("%d runs after a second Close, want %d", n, len(calls))
	}
}

func TestCloseWithoutChange(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.c.Close()
	if calls := h.f.all(); len(calls) != 0 {
		t.Errorf("runs = %q, want none", calls)
	}
}

func TestNoGoroutineLeft(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.green()
	h.c.Close()
	select {
	case <-h.c.done:
	default:
		t.Error("the worker is still running after Close")
	}
}

func TestZeroConfig(t *testing.T) {
	c := New(Config{}) // no runner: nothing to drive, and nothing to crash
	for range 5 {
		c.Offer(Red)
	}
	c.Close()
	var nilCtl *Controller
	nilCtl.Offer(Red)
	if nilCtl.Note() != "" {
		t.Error("nil controller has a note")
	}
	nilCtl.Close()
}

// Hovering on a limit must not leave a calmer colour showing: when every one of
// the last three polls was worse than what is shown, the mildest of them is shown.
func TestFlappingAboveShownEscalates(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.green()
	for i := range 6 {
		h.c.Offer([]State{Red, Amber}[i%2])
	}
	h.wait()
	h.c.Close()
	h.wantColours("00FF00", "FF7E00", "off")
}

// And the other way: when every one of the last three polls was better than what
// is shown, the worst of them is shown.
func TestFlappingBelowShownRecovers(t *testing.T) {
	h := start(t, newFake(t), nil)
	h.offer(Red, 3)
	h.wait()
	for i := range 6 {
		h.c.Offer([]State{Green, Amber}[i%2])
	}
	h.wait()
	h.c.Close()
	h.wantColours("FF0000", "FF7E00", "off")
}
