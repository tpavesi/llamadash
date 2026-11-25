package collect

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llamadash/internal/hwmon"
)

func hwmonStub(calls *int32, r hwmon.Reading, err error) func() (hwmon.Reading, error) {
	return func() (hwmon.Reading, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return r, err
	}
}

// hwmonSeq returns the readings in turn, repeating the last.
type hwmonSeq struct {
	mu    sync.Mutex
	steps []hwmon.Reading
	errs  []error
	i     int
}

func (q *hwmonSeq) read() (hwmon.Reading, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := min(q.i, len(q.steps)-1)
	q.i++
	var err error
	if i < len(q.errs) {
		err = q.errs[i]
	}
	return q.steps[i], err
}

// fan is fan "fan N" of the nct6798, its identity nct6798:fanN.
func fan(n string, rpm int) hwmon.Fan {
	return hwmon.Fan{Key: "nct6798:fan" + strings.TrimPrefix(n, "fan "), Chip: "nct6798", Label: n, RPM: rpm}
}

func shown(fs []Fan) map[string]Fan {
	m := map[string]Fan{}
	for _, f := range fs {
		m[f.Label] = f
	}
	return m
}

func TestFansShownOnceSpinning(t *testing.T) {
	q := &hwmonSeq{steps: []hwmon.Reading{
		{Fans: []hwmon.Fan{fan("fan 1", 1250), fan("fan 2", 0), fan("fan 3", 0)}, CPUTempC: 60.125, HasCPUTemp: true},
		{Fans: []hwmon.Fan{fan("fan 1", 1200), fan("fan 2", 0), fan("fan 3", 700)}},
		{Fans: []hwmon.Fan{fan("fan 1", 0), fan("fan 2", 0), fan("fan 3", 650)}},
	}}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.GPU, c.Hwmon = GPUAuto, q.read })
	s := c.Poll(context.Background(), t0)
	m := shown(s.Fans)
	if len(s.Fans) != 1 || m["fan 1"].RPM != 1250 || m["fan 1"].Stopped || m["fan 1"].Chip != "nct6798" {
		t.Fatalf("poll 1: %+v", s.Fans)
	}
	if !s.HasCPUTemp || s.CPUTempC != 60.125 || !s.HwmonAt.Equal(t0) {
		t.Fatalf("poll 1 temp: %v %v %v", s.CPUTempC, s.HasCPUTemp, s.HwmonAt)
	}
	s = c.Poll(context.Background(), t0.Add(time.Second))
	if len(s.Fans) != 2 || shown(s.Fans)["fan 3"].RPM != 700 || s.HasCPUTemp {
		t.Fatalf("poll 2: %+v temp %v", s.Fans, s.HasCPUTemp)
	}
	s = c.Poll(context.Background(), t0.Add(2*time.Second))
	m = shown(s.Fans)
	if len(s.Fans) != 2 || !m["fan 1"].Stopped || m["fan 1"].RPM != 0 || m["fan 3"].Stopped {
		t.Fatalf("poll 3: a fan that stopped must stay, flagged: %+v", s.Fans)
	}
	if s.Fans[0].Label != "fan 1" || s.Fans[1].Label != "fan 3" {
		t.Errorf("reading order not kept: %+v", s.Fans)
	}
	// the snapshot owns its slice
	s.Fans[0].Label = "x"
	if s2 := c.Poll(context.Background(), t0.Add(3*time.Second)); s2.Fans[0].Label != "fan 1" {
		t.Errorf("snapshot shares the collector's slice")
	}
}

func TestFansUnavailableRetries(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.Hwmon = GPUAuto, hwmonStub(&calls, hwmon.Reading{}, hwmon.ErrUnavailable)
	})
	for i := 0; i <= 125; i++ {
		if s := c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second)); s.Fans != nil || s.HasCPUTemp {
			t.Fatalf("readings while unavailable: %+v", s.Fans)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 { // 0s, 60s, 120s
		t.Fatalf("reads = %d, want 3", n)
	}
	// a driver loaded later is picked up at the next retry; when the tree goes
	// away again the fan it showed stays, as missing (n/a)
	q := &hwmonSeq{
		steps: []hwmon.Reading{{}, {Fans: []hwmon.Fan{fan("fan 1", 900)}}, {}},
		errs:  []error{hwmon.ErrUnavailable, nil, hwmon.ErrUnavailable},
	}
	c = newC("http://127.0.0.1:1", func(c *Config) { c.GPU, c.Hwmon = GPUAuto, q.read })
	c.Poll(context.Background(), t0)
	if s := c.Poll(context.Background(), t0.Add(60*time.Second)); len(s.Fans) != 1 {
		t.Fatalf("not picked up: %+v", s.Fans)
	}
	if s := c.Poll(context.Background(), t0.Add(61*time.Second)); len(s.Fans) != 1 || !s.Fans[0].Missing || s.HasCPUTemp {
		t.Fatalf("not shown as missing: %+v", s.Fans)
	}
}

func TestFansGate(t *testing.T) {
	var calls int32
	mut := func(c *Config) {
		c.Hwmon = hwmonStub(&calls, hwmon.Reading{Fans: []hwmon.Fan{fan("fan 1", 900)}}, nil)
	}
	newC("http://192.0.2.10:8011", func(c *Config) { mut(c); c.GPU = GPUAuto }).Poll(context.Background(), t0)
	newC("http://127.0.0.1:1", func(c *Config) { mut(c); c.GPU = GPUOff }).Poll(context.Background(), t0)
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("read %d times outside the local-sources gate", n)
	}
	s := newC("http://192.0.2.10:8011", func(c *Config) { mut(c); c.GPU = GPUOn }).Poll(context.Background(), t0)
	if atomic.LoadInt32(&calls) != 1 || len(s.Fans) != 1 {
		t.Fatalf("--gpu: %d reads, %+v", calls, s.Fans)
	}
}

func TestFansHungReadStartedOnce(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Hwmon = func() (hwmon.Reading, error) { atomic.AddInt32(&calls, 1); <-release; return hwmon.Reading{}, nil }
	})
	start := time.Now()
	for i := 0; i < 3; i++ {
		c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	close(release)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Poll blocked for %v", d)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("hung read started %d times", n)
	}
}

func TestFansSeenBounded(t *testing.T) {
	n := 0
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Hwmon = func() (hwmon.Reading, error) { // a new key every read
			n++
			return hwmon.Reading{Fans: []hwmon.Fan{{Key: time.Duration(n).String(), Label: "f", RPM: 500}}}, nil
		}
	})
	for i := 0; i < 3*maxSeenFans; i++ {
		c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(c.loc.hw.spun) > maxSeenFans {
		t.Fatalf("%d keys remembered", len(c.loc.hw.spun))
	}
}

// Fans remembered from earlier runs are shown from the first reading: one that
// reads 0 as stopped, one whose input is gone as missing (n/a).
func TestKnownFansSeeded(t *testing.T) {
	var reported [][]string
	var mu sync.Mutex
	q := &hwmonSeq{steps: []hwmon.Reading{
		{Fans: []hwmon.Fan{fan("fan 1", 1250), fan("fan 2", 0), fan("fan 3", 0)}},
		{Fans: []hwmon.Fan{fan("fan 1", 1250), fan("fan 2", 0), fan("fan 3", 900)}},
		{Fans: []hwmon.Fan{fan("fan 1", 1250), fan("fan 2", 0), fan("fan 3", 900)}},
	}}
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.Hwmon = GPUAuto, q.read
		c.KnownFans = []string{"nct6798:fan1", "nct6798:fan2", "nct6798#2:fan4"}
		c.OnNewFans = func(ids []string) { mu.Lock(); reported = append(reported, ids); mu.Unlock() }
	})
	s := c.Poll(context.Background(), t0)
	m := shown(s.Fans)
	if len(s.Fans) != 3 || !m["fan 2"].Stopped || m["fan 1"].Stopped || m["fan 1"].RPM != 1250 ||
		!m["fan 4"].Missing || m["fan 4"].Chip != "nct6798#2" || m["fan 4"].Stopped {
		t.Fatalf("first reading: %+v", s.Fans)
	}
	if len(reported) != 0 {
		t.Fatalf("known fans reported as new: %v", reported)
	}
	s = c.Poll(context.Background(), t0.Add(time.Second)) // fan 3 spins for the first time
	s = c.Poll(context.Background(), t0.Add(2*time.Second))
	if len(s.Fans) != 4 || shown(s.Fans)["fan 3"].RPM != 900 {
		t.Fatalf("fan 3: %+v", s.Fans)
	}
	want := []string{"nct6798:fan1", "nct6798:fan2", "nct6798#2:fan4", "nct6798:fan3"}
	if len(reported) != 1 || !slices.Equal(reported[0], want) {
		t.Fatalf("reported %v, want once %v", reported, want)
	}
}

// Without any usable sensor (driver not loaded) the known fans still show, n/a.
func TestKnownFansWithoutSensors(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Hwmon = hwmonStub(nil, hwmon.Reading{}, hwmon.ErrUnavailable)
		c.KnownFans = []string{"it87:fan1", "it87:fan2"}
	})
	s := c.Poll(context.Background(), t0)
	if len(s.Fans) != 2 || !s.Fans[0].Missing || !s.Fans[1].Missing || s.Fans[1].Label != "fan 2" || !s.HwmonAt.Equal(t0) {
		t.Fatalf("%+v at %v", s.Fans, s.HwmonAt)
	}
	// none known: nothing, as before
	c = newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.Hwmon = GPUAuto, hwmonStub(nil, hwmon.Reading{}, errors.Join(hwmon.ErrUnavailable))
	})
	if s := c.Poll(context.Background(), t0); s.Fans != nil {
		t.Fatalf("%+v", s.Fans)
	}
}

// No more than maxSeenFans fans are known, seeds included.
func TestKnownFansCapped(t *testing.T) {
	var ids []string
	for i := range maxSeenFans + 10 {
		ids = append(ids, "c:fan"+strings.Repeat("1", 1)+string(rune('0'+i%10))+string(rune('a'+i/10)))
	}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.KnownFans = ids })
	if len(c.loc.hw.known) != maxSeenFans || len(c.loc.hw.spun) != maxSeenFans {
		t.Fatalf("%d known", len(c.loc.hw.known))
	}
}
