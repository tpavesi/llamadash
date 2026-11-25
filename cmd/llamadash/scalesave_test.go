package main

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llamadash/internal/ui"
)

// fakeClock is a settable clock whose timers fire when the test says so.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	waits  []time.Duration
	timers []chan time.Time
	asked  chan struct{} // one value per timer started
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), asked: make(chan struct{}, 100)}
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	c.waits, c.timers = append(c.waits, d), append(c.timers, ch)
	c.mu.Unlock()
	c.asked <- struct{}{}
	return ch
}

// fire advances the clock by the last wait and fires its timer.
func (c *fakeClock) fire(t *testing.T) time.Duration {
	t.Helper()
	select {
	case <-c.asked:
	case <-time.After(2 * time.Second):
		t.Fatal("no timer started")
	}
	c.mu.Lock()
	d, ch := c.waits[len(c.waits)-1], c.timers[len(c.timers)-1]
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch <- now
	return d
}

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// saveRec records the writes; fail makes them fail.
type saveRec struct {
	mu    sync.Mutex
	calls [][]confVal
	fail  bool
	wrote chan struct{}
}

func (r *saveRec) save(path string, v []confVal) error {
	r.mu.Lock()
	r.calls = append(r.calls, v)
	fail := r.fail
	r.mu.Unlock()
	r.wrote <- struct{}{}
	if fail {
		return errors.New("read-only file system")
	}
	return nil
}

func (r *saveRec) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

func (r *saveRec) last() []confVal { r.mu.Lock(); defer r.mu.Unlock(); return r.calls[len(r.calls)-1] }

func waitWrite(t *testing.T, r *saveRec) {
	t.Helper()
	select {
	case <-r.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("no write")
	}
}

func newTestSaver(psu float64) (*scaleSaver, *fakeClock, *saveRec) {
	clk, rec := newFakeClock(), &saveRec{wrote: make(chan struct{}, 100)}
	return newScaleSaver("/x/llamadash.conf", psu, rec.save, clk.Now, clk.After), clk, rec
}

// The first raise is written at once, later ones at most every 30 s, coalesced,
// and what is pending is written on quit.
func TestScaleSaverCoalesces(t *testing.T) {
	s, clk, rec := newTestSaver(1000)
	s.Offer(ui.Scales{PowerW: 1000, GenTPS: 40})
	waitWrite(t, rec)
	// the power top is the PSU's: nothing learned for it
	if got := rec.last(); len(got) != 1 || got[0] != (numVal("LLAMADASH_AXIS_GEN_TPS", 40)) {
		t.Fatalf("first write %v", got)
	}
	s.Offer(ui.Scales{PowerW: 1000, GenTPS: 50})
	s.Offer(ui.Scales{PowerW: 1500, GenTPS: 60, PromptTPS: 800})
	if d := clk.fire(t); d != 30*time.Second {
		t.Fatalf("waited %v", d)
	}
	waitWrite(t, rec)
	want := []confVal{numVal("LLAMADASH_AXIS_POWER_W", 1500), numVal("LLAMADASH_AXIS_GEN_TPS", 60), numVal("LLAMADASH_AXIS_PROMPT_TPS", 800)}
	if got := rec.last(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("coalesced write %v", got)
	}
	// the second Offer's kick may start one more wait; nothing is pending for it
	clk.advance(5 * time.Second)
	s.Offer(ui.Scales{PowerW: 1500, GenTPS: 80, PromptTPS: 800})
	s.Close()
	if rec.n() != 3 || rec.last()[1] != (numVal("LLAMADASH_AXIS_GEN_TPS", 80)) {
		t.Fatalf("quit: %d writes, last %v", rec.n(), rec.last())
	}
	s.Close() // twice is fine
	s.Offer(ui.Scales{GenTPS: 100})
	if rec.n() != 3 {
		t.Fatal("a write after Close")
	}
}

// After a failed write nothing is tried for 10 minutes, not even on quit.
func TestScaleSaverBacksOffAfterFailure(t *testing.T) {
	s, clk, rec := newTestSaver(0)
	rec.fail = true
	s.Offer(ui.Scales{GenTPS: 40})
	waitWrite(t, rec)
	s.Offer(ui.Scales{GenTPS: 50})
	if d := clk.fire(t); d != 10*time.Minute {
		t.Fatalf("retry after %v", d)
	}
	waitWrite(t, rec)
	if rec.n() != 2 || rec.last()[0].val != "50" {
		t.Fatalf("retry: %d writes %v", rec.n(), rec.last())
	}
	clk.advance(time.Minute)
	s.Close() // failed a minute ago: no write on quit
	if rec.n() != 2 {
		t.Fatalf("wrote on quit within the back-off: %d", rec.n())
	}
}

func TestScaleSaverQuitWithoutChanges(t *testing.T) {
	s, _, rec := newTestSaver(0)
	s.Close()
	if rec.n() != 0 {
		t.Fatal("wrote with nothing learned")
	}
}

// The real thing end to end: the settings file named by --config gets the lines.
func TestScaleSaverWritesTheFile(t *testing.T) {
	p := writeConf(t, "LLAMADASH_URL=http://localhost:8011\r\n")
	s := newScaleSaver(p, 0, saveConf, time.Now, after)
	s.Offer(ui.Scales{GenTPS: 87.5})
	s.Close()
	got, _ := os.ReadFile(p)
	if string(got) != "LLAMADASH_URL=http://localhost:8011\r\n"+scalesComment+"\r\nLLAMADASH_AXIS_GEN_TPS=87.5\r\n" {
		t.Fatalf("%q", got)
	}
	o, _, code := parseArgs([]string{"--config", p}, envFn(nil), &bytes.Buffer{})
	if code != -1 || o.axisGen != 87.5 {
		t.Fatalf("read back: code %d %+v", code, o)
	}
}

func TestScaleSettings(t *testing.T) {
	p := writeConf(t, "LLAMADASH_PSU_WATTS=850\nLLAMADASH_AXIS_POWER_W=1200\nLLAMADASH_AXIS_GEN_TPS=87.5\nLLAMADASH_AXIS_PROMPT_TPS=1500\n")
	o, _, code := parseArgs([]string{"--config", p}, envFn(nil), &bytes.Buffer{})
	if code != -1 || o.psuWatts != 850 || o.axisPower != 1200 || o.axisGen != 87.5 || o.axisPrompt != 1500 || o.confFile != p {
		t.Fatalf("code %d %+v", code, o)
	}
	if sc := o.scales(); sc != (ui.Scales{PowerW: 1200, GenTPS: 87.5, PromptTPS: 1500}) {
		t.Fatalf("scales %+v", sc)
	}
	// flag > env > file for the PSU
	o, _, _ = parseArgs([]string{"--config", p, "--psu-watts", "1000"}, envFn(map[string]string{"LLAMADASH_PSU_WATTS": "900"}), &bytes.Buffer{})
	if o.psuWatts != 1000 || o.scales().PowerW != 1200 {
		t.Fatalf("flag: %+v", o)
	}
	o, _, _ = parseArgs([]string{"--config", p}, envFn(map[string]string{"LLAMADASH_PSU_WATTS": "1600"}), &bytes.Buffer{})
	if o.psuWatts != 1600 || o.scales().PowerW != 1600 {
		t.Fatalf("env: %+v", o)
	}
	// no settings file: nothing to write to
	if o, _, _ := parseArgs([]string{"--psu-watts", "1000"}, envFn(nil), &bytes.Buffer{}); o.confFile != "" || o.scales().PowerW != 1000 {
		t.Fatalf("no file: %+v", o)
	}
	// a learned top the program wrote is never fatal: ignored with a warning,
	// the others still apply, and the next save overwrites it
	for _, bad := range []string{"LLAMADASH_AXIS_POWER_W=0", "LLAMADASH_AXIS_GEN_TPS=-1", "LLAMADASH_AXIS_PROMPT_TPS=2e9",
		"LLAMADASH_AXIS_GEN_TPS=1e12", "LLAMADASH_AXIS_GEN_TPS=Inf", "LLAMADASH_AXIS_GEN_TPS=abc", "LLAMADASH_AXIS_GEN_TPS=NaN"} {
		var errb bytes.Buffer
		key := strings.SplitN(bad, "=", 2)[0]
		o, _, code := parseArgs([]string{"--config", writeConf(t, bad+"\nLLAMADASH_PSU_WATTS=900\n")}, envFn(nil), &errb)
		lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
		if code != -1 || len(lines) != 1 || !strings.Contains(lines[0], key) || !strings.Contains(lines[0], "ignor") ||
			o.axisPower != 0 || o.axisGen != 0 || o.axisPrompt != 0 || o.psuWatts != 900 || o.scales().PowerW != 900 {
			t.Errorf("%s: code %d stderr %q %+v", bad, code, errb.String(), o)
		}
	}
	for _, bad := range []string{"LLAMADASH_PSU_WATTS=0", "LLAMADASH_PSU_WATTS=100001", "LLAMADASH_PSU_WATTS=abc",
		"LLAMADASH_PSU_WATTS=NaN"} {
		var errb bytes.Buffer
		key := strings.SplitN(bad, "=", 2)[0]
		if _, _, code := parseArgs([]string{"--config", writeConf(t, bad+"\n")}, envFn(nil), &errb); code != 2 || !strings.Contains(errb.String(), key) {
			t.Errorf("%s: code %d stderr %q", bad, code, errb.String())
		}
	}
	var errb bytes.Buffer
	if _, _, code := parseArgs([]string{"--psu-watts", "0.5"}, envFn(nil), &errb); code != 2 || !strings.Contains(errb.String(), "psu-watts") {
		t.Errorf("--psu-watts 0.5: code %d stderr %q", code, errb.String())
	}
}

// Which file is written: the named one, else the user's file when it exists,
// else the system file when it exists.
func TestScaleFileChoice(t *testing.T) {
	home := t.TempDir()
	user := filepath.Join(home, ".config", "llamadash", "llamadash.conf")
	env := envFn(map[string]string{"HOME": home})
	o, _, _ := parseArgs(nil, env, &bytes.Buffer{})
	if o.confFile != "" {
		t.Fatalf("no files: %q", o.confFile)
	}
	old := readFile
	defer func() { readFile = old }()
	readFile = func(p string) ([]byte, error) {
		if p == systemConfig {
			return []byte("LLAMADASH_CURRENCY=GBP\n"), nil
		}
		return os.ReadFile(p)
	}
	if o, _, _ := parseArgs(nil, env, &bytes.Buffer{}); o.confFile != systemConfig {
		t.Fatalf("system file only: %q", o.confFile)
	}
	if err := os.MkdirAll(filepath.Dir(user), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := parseArgs(nil, env, &bytes.Buffer{}); o.confFile != user {
		t.Fatalf("user file: %q", o.confFile)
	}
	named := writeConf(t, "")
	if o, _, _ := parseArgs([]string{"--config", named}, env, &bytes.Buffer{}); o.confFile != named {
		t.Fatalf("--config: %q", o.confFile)
	}
}

// One-shot runs never write the settings file.
func TestOnceWritesNoSettings(t *testing.T) {
	p := writeConf(t, "LLAMADASH_URL=http://127.0.0.1:1\n")
	if s := scaleSink(options{confFile: p, once: true}); s != nil {
		t.Fatal("--once has a saver")
	}
	if s := scaleSink(options{confFile: p, frameW: 80, frameH: 24}); s != nil {
		t.Fatal("--frame has a saver")
	}
	if s := scaleSink(options{}); s != nil {
		t.Fatal("a saver without a settings file")
	}
	s := scaleSink(options{confFile: p})
	if s == nil {
		t.Fatal("no saver for the dashboard")
	}
	s.Close()
	code, _, _ := runArgs([]string{"--once", "--no-gpu", "--no-db", "--interval", "250ms", "--config", p}, nil)
	if got, _ := os.ReadFile(p); code != 1 || string(got) != "LLAMADASH_URL=http://127.0.0.1:1\n" {
		t.Fatalf("code %d file %q", code, got)
	}
}

// A top the reader would reject is never written.
func TestScaleSaverSkipsInvalidTops(t *testing.T) {
	s, _, rec := newTestSaver(0)
	s.Offer(ui.Scales{PowerW: math.Inf(1), GenTPS: 1e12, PromptTPS: 800})
	waitWrite(t, rec)
	s.Close()
	if got := rec.last(); len(got) != 1 || got[0] != (numVal("LLAMADASH_AXIS_PROMPT_TPS", 800)) {
		t.Fatalf("wrote %v", got)
	}
}
