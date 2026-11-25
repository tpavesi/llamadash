package collect

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llamadash/internal/cpupower"
	"llamadash/internal/fakeserver"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/procstat"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

type usageCall struct {
	at time.Time
	u  store.Usage
}

// fakeRec is a Recorder that records every call.
type fakeRec struct {
	mu       sync.Mutex
	samples  []store.Sample
	requests []stats.Finished
	usage    []usageCall
	tariffs  []float64
	err      error
	recent   []stats.Finished
	today    store.Totals

	totalsCalls int32
	totalsGate  chan struct{} // when non-nil, Totals waits for it to close
}

func (f *fakeRec) SetTariff(_ time.Time, p float64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tariffs = append(f.tariffs, p)
	return nil
}
func (f *fakeRec) AddSample(x store.Sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, x)
}
func (f *fakeRec) AddRequest(x stats.Finished) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, x)
}
func (f *fakeRec) AddUsage(at time.Time, u store.Usage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = append(f.usage, usageCall{at, u})
}
func (f *fakeRec) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func (f *fakeRec) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}
func (f *fakeRec) RecentRequests(n int) ([]stats.Finished, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]stats.Finished(nil), f.recent[:min(n, len(f.recent))]...), nil
}
func (f *fakeRec) Totals(time.Time) (today, month, all store.Totals, err error) {
	atomic.AddInt32(&f.totalsCalls, 1)
	if f.totalsGate != nil {
		<-f.totalsGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.today, f.today, f.today, nil
}

func (f *fakeRec) sumUsage() store.Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s store.Usage
	for _, c := range f.usage {
		s.GenTokens += c.u.GenTokens
		s.PromptTokens += c.u.PromptTokens
		s.Requests += c.u.Requests
		s.GPUEnergyWh += c.u.GPUEnergyWh
		s.CPUEnergyWh += c.u.CPUEnergyWh
		s.Cost += c.u.Cost
	}
	return s
}

// lockedErr stands in for the SQLite driver's error type (it carries a result code).
type lockedErr struct{}

func (lockedErr) Error() string { return "database is locked (5) (SQLITE_BUSY)" }
func (lockedErr) Code() int     { return 5 }

func near(got, want, rel float64) bool { return math.Abs(got-want) <= math.Abs(want)*rel }

// genSteps returns slots responses whose decoded counter rises by incs[i] at step i+1.
func genSteps(incs ...int) []fakeserver.Step {
	d := 0
	steps := []fakeserver.Step{step(propsOff, ok(slotsBody(0)), ok(nil))}
	for _, n := range incs {
		d += n
		steps = append(steps, step(propsOff, ok(slotsBody(d)), ok(nil)))
	}
	return steps
}

func TestWindowSampleEvery10s(t *testing.T) {
	var incs []int
	for i := 0; i < 24; i++ {
		incs = append(incs, 10+7*(i%5)) // uneven, so the smoothed rate varies
	}
	srv := fakeserver.New(genSteps(incs...))
	defer srv.Close()
	rec := &fakeRec{}
	c := newC(srv.URL, func(c *Config) { c.Store = rec })
	var s [25]Snapshot
	for i := range s {
		s[i] = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(rec.samples) != 2 {
		t.Fatalf("%d samples, want 2: %+v", len(rec.samples), rec.samples)
	}
	for w := 0; w < 2; w++ {
		want := 0.0
		for i := w * 10; i < w*10+10; i++ {
			want += s[i].GenRate / 10
		}
		if got := rec.samples[w].GenRate; math.Abs(got-want) > 1e-9 || want == 0 {
			t.Errorf("sample %d GenRate %v, want %v", w, got, want)
		}
		if at := rec.samples[w].At; !at.Equal(t0.Add(time.Duration(w*10) * time.Second)) {
			t.Errorf("sample %d at %v", w, at)
		}
		if p := rec.samples[w].Processing; p != 1 {
			t.Errorf("sample %d Processing %d, want 1", w, p)
		}
	}
	if len(rec.usage) != 2 || !rec.usage[0].at.Equal(t0.Add(10*time.Second)) || !rec.usage[1].at.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("usage: %+v", rec.usage)
	}
	want := 0 // tokens of polls 1..19 (poll 0 is the first sight)
	for _, n := range incs[:19] {
		want += n
	}
	if got := rec.sumUsage().GenTokens; got != int64(want) {
		t.Errorf("GenTokens %d, want %d", got, want)
	}
}

// A read-only collector that starts recording drops the window it was in, so
// nothing from the read-only period is written, and DBReadOnly clears.
func TestStartRecordingDropsReadOnlyWindow(t *testing.T) {
	var incs []int
	for i := 0; i < 30; i++ {
		incs = append(incs, 10)
	}
	srv := fakeserver.New(genSteps(incs...))
	defer srv.Close()
	rec := &fakeRec{}
	writing := false
	gate := &gatedRec{fakeRec: rec, on: &writing}
	c := newC(srv.URL, func(c *Config) { c.Store, c.DBReadOnly = gate, true })
	poll := func(i int) Snapshot { return c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second)) }
	for i := 0; i <= 15; i++ { // one window dropped by the read-only store, one open
		if s := poll(i); !s.DBReadOnly {
			t.Fatalf("poll %d: not read-only", i)
		}
	}
	c.StartRecording(func() { writing = true })
	for i := 16; i <= 26; i++ {
		if s := poll(i); s.DBReadOnly {
			t.Fatalf("poll %d: still read-only", i)
		}
	}
	if len(rec.samples) != 1 || !rec.samples[0].At.Equal(t0.Add(16*time.Second)) {
		t.Fatalf("samples: %+v", rec.samples)
	}
	if u := rec.sumUsage(); u.GenTokens != 100 { // polls 17..26: 10 polls of 10 tokens
		t.Fatalf("usage %+v", u)
	}
}

// gatedRec drops writes while *on is false, like the read-only store.
type gatedRec struct {
	*fakeRec
	on *bool
}

func (g *gatedRec) AddSample(x store.Sample) {
	if *g.on {
		g.fakeRec.AddSample(x)
	}
}
func (g *gatedRec) AddRequest(x stats.Finished) {
	if *g.on {
		g.fakeRec.AddRequest(x)
	}
}
func (g *gatedRec) AddUsage(at time.Time, u store.Usage) {
	if *g.on {
		g.fakeRec.AddUsage(at, u)
	}
}

// A window without GPU, CPU power, host or process readings sends NaN (and no host
// or process memory) for them, not 0.
func TestWindowWithoutReadingsIsNoData(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec }) // every local source unavailable
	for i := 0; i <= 10; i++ {
		c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(rec.samples) != 1 {
		t.Fatalf("%d samples", len(rec.samples))
	}
	x := rec.samples[0]
	for name, v := range map[string]float64{"gpu_w": x.GPUPowerW, "cpu_w": x.CPUPowerW, "gpu_util": x.GPUUtil,
		"gpu_mem_pct": x.GPUMemPct, "gpu_temp": x.GPUTempC, "host_cpu": x.HostCPUPct, "proc_cpu": x.ProcCPUPct} {
		if !math.IsNaN(v) {
			t.Errorf("%s = %v, want NaN", name, v)
		}
	}
	if x.HasHostMem || x.HasProcRSS || math.IsNaN(x.GenRate) {
		t.Errorf("sample %+v", x)
	}
}

// With the sources present the same window carries their values.
func TestWindowWithReadings(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.Store, c.GPU = rec, GPUOn
		c.GPUQuery = powerSeq([]gpu.GPU{{PowerW: 300, HasPower: true, UtilPct: 50, HasUtil: true, TempC: 60, HasTemp: true,
			MemUsedMiB: 10, MemTotalMiB: 20}})
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
		c.Host = hostStub(nil, host.Info{CPUPct: 4, HasCPU: true, MemTotalKB: 100, MemAvailKB: 40}, nil)
		c.Proc = procStub(nil, procstat.Info{PID: 1, CPUPct: 90, HasCPU: true, RSSKB: 7}, nil)
	})
	for i := 0; i <= 10; i++ {
		pollGPU(t, c, t0.Add(time.Duration(i)*time.Second))
	}
	if len(rec.samples) != 1 {
		t.Fatalf("%d samples", len(rec.samples))
	}
	x := rec.samples[0]
	if x.GPUPowerW != 300 || x.CPUPowerW != 100 || x.GPUUtil != 50 || x.GPUTempC != 60 || x.GPUMemPct != 50 ||
		x.HostCPUPct != 4 || !x.HasHostMem || x.HostMemUsedKB != 60 || x.ProcCPUPct != 90 || !x.HasProcRSS || x.ProcRSSKB != 7 {
		t.Errorf("sample %+v", x)
	}
}

// constPower returns a GPU query that reports w watts for n calls, then no power.
// Queries run every 2s (gpuEvery), so n calls cover samples at 0, 2, ... 2(n-1) s.
func constPower(w float64, n int) func(context.Context) ([]gpu.GPU, error) {
	seq := make([][]gpu.GPU, 0, n+1)
	for i := 0; i < n; i++ {
		seq = append(seq, pw(w))
	}
	return powerSeq(append(seq, []gpu.GPU{{}})...)
}

func TestUsageCostUsesPrice(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, constPower(300, 11) // samples at 0, 2, ... 20s
		c.Store, c.KWhPrice, c.Currency = rec, 0.3031, "GBP"
	})
	var s Snapshot
	for i := 0; i <= 30; i++ {
		s = pollGPU(t, c, t0.Add(time.Duration(i)*time.Second))
	}
	u := rec.sumUsage()
	if !near(u.GPUEnergyWh, 300*20.0/3600, 0.01) || !near(u.Cost, 0.000505, 0.01) {
		t.Fatalf("usage %+v", u)
	}
	if u.GPUEnergyWh != s.GPUEnergyWh { // the same integration as the on-screen figure
		t.Fatalf("usage energy %v != snapshot %v", u.GPUEnergyWh, s.GPUEnergyWh)
	}
}

func TestCostSnapshot(t *testing.T) {
	incs := make([]int, 10)
	for i := range incs {
		incs[i] = 400
	}
	for _, price := range []float64{0.10, 0} {
		srv := fakeserver.New(genSteps(incs...))
		rec := &fakeRec{}
		c := newC(srv.URL, func(c *Config) {
			c.GPU, c.GPUQuery = GPUOn, constPower(360, 6) // samples at 0, 2, ... 10s
			c.Store, c.KWhPrice, c.Currency = rec, price, "GBP"
		})
		var s Snapshot
		for i := 0; i <= 21; i++ {
			s = pollGPU(t, c, t0.Add(time.Duration(i)*time.Second))
		}
		srv.Close()
		if s.GenTokensSinceStart != 4000 {
			t.Fatalf("price %v: GenTokensSinceStart %d", price, s.GenTokensSinceStart)
		}
		if price == 0 {
			if s.Cost.Enabled || s.Cost.SinceStart != 0 || rec.sumUsage().Cost != 0 {
				t.Fatalf("price 0: %+v usage %+v", s.Cost, rec.sumUsage())
			}
			continue
		}
		cs := s.Cost
		if !cs.Enabled || cs.Price != 0.10 || cs.Currency != "GBP" || !near(cs.SinceStart, 0.0001, 0.01) ||
			!near(cs.PerDay, 0.864, 0.01) || !near(cs.PerMTok, 0.025, 0.01) {
			t.Fatalf("cost %+v", cs)
		}
		if u := rec.sumUsage(); !near(u.Cost, 0.0001, 0.01) {
			t.Fatalf("usage cost %+v", u)
		}
	}
}

// cpuSeq returns the CPU power readings in order, then the last one again.
func cpuSeq(rs ...cpuResult) func(time.Time) (float64, float64, bool, error) {
	var i int32
	return func(time.Time) (float64, float64, bool, error) {
		r := rs[min(int(atomic.AddInt32(&i, 1))-1, len(rs)-1)]
		return r.watts, r.joules, r.ok, r.err
	}
}

// After two polls (--once, --frame, the first second of the TUI) a source with
// power but no integrated interval yet counts at its current draw in PerDay.
func TestPerDayAtCurrentDrawAfterTwoPolls(t *testing.T) {
	const price = 0.3031
	noCPU := cpuStub(nil, 0, 0, false, cpupower.ErrUnavailable)
	cpu := func() func(time.Time) (float64, float64, bool, error) { // the first read only primes
		return cpuSeq(cpuResult{}, cpuResult{watts: 118, joules: 118, ok: true})
	}
	for _, c := range []struct {
		name  string
		gpu   func(context.Context) ([]gpu.GPU, error)
		cpu   func(time.Time) (float64, float64, bool, error)
		watts float64
	}{
		{"gpu only", powerSeq(pw(460)), noCPU, 460},
		{"cpu only", powerSeq(nil), cpu(), 118},
		{"both", powerSeq(pw(460)), cpu(), 578},
	} {
		col := newC("http://127.0.0.1:1", func(cf *Config) {
			cf.GPU, cf.GPUQuery, cf.CPUPower, cf.KWhPrice = GPUOn, c.gpu, c.cpu, price
		})
		pollGPU(t, col, t0)
		s := col.Poll(context.Background(), t0.Add(time.Second))
		if s.PowerW != c.watts || !near(s.Cost.PerDay, c.watts/1000*24*price, 1e-9) {
			t.Errorf("%s: power %v W, PerDay %v, want %v", c.name, s.PowerW, s.Cost.PerDay, c.watts/1000*24*price)
		}
		if s.OneShot {
			t.Errorf("%s: OneShot without Config.OneShot", c.name)
		}
	}
	if s := newC("http://127.0.0.1:1", func(c *Config) { c.OneShot = true }).Poll(context.Background(), t0); !s.OneShot {
		t.Error("Config.OneShot does not reach the snapshot")
	}
}

func TestPerMTokNeedsThousandTokens(t *testing.T) {
	srv := fakeserver.New(genSteps(400, 400))
	defer srv.Close()
	c := newC(srv.URL, func(c *Config) {
		c.GPU, c.GPUQuery, c.KWhPrice = GPUOn, constPower(360, 100), 0.10
	})
	var s Snapshot
	for i := 0; i < 5; i++ {
		s = pollGPU(t, c, t0.Add(time.Duration(i)*time.Second))
	}
	if s.GenTokensSinceStart != 800 || s.Cost.PerMTok != 0 || s.Cost.SinceStart <= 0 {
		t.Fatalf("tokens %d cost %+v", s.GenTokensSinceStart, s.Cost)
	}
}

func busyIdleSteps(n int) []fakeserver.Step {
	busy := func(task int) []byte {
		return []byte(fmt.Sprintf(`[{"id":0,"n_ctx":4096,"is_processing":true,"id_task":%d,"n_prompt_tokens":100,"n_prompt_tokens_processed":10,"n_prompt_tokens_cache":90,"next_token":[{"n_decoded":5,"n_remain":-1}]}]`, task))
	}
	idle := func(task int) []byte {
		return []byte(fmt.Sprintf(`[{"id":0,"n_ctx":4096,"is_processing":false,"id_task":%d,"n_prompt_tokens":105,"n_prompt_tokens_processed":0,"n_prompt_tokens_cache":0,"next_token":[{"n_decoded":0,"n_remain":-1}]}]`, task))
	}
	var steps []fakeserver.Step
	for i := 1; i <= n; i++ {
		steps = append(steps, step(propsOff, ok(busy(100+i)), ok(nil)), step(propsOff, ok(idle(100+i)), ok(nil)))
	}
	return steps
}

func TestRequestsForwardedOnce(t *testing.T) {
	srv := fakeserver.New(busyIdleSteps(3))
	defer srv.Close()
	rec := &fakeRec{}
	c := newC(srv.URL, func(c *Config) { c.Store = rec })
	for i := 0; i <= 20; i++ {
		c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(rec.requests) != 3 || rec.sumUsage().Requests != 3 {
		t.Fatalf("requests %d, usage %+v", len(rec.requests), rec.sumUsage())
	}
	for i, f := range rec.requests {
		if f.TaskID != 101+i {
			t.Fatalf("request %d: %+v", i, f)
		}
	}
}

func TestSeedRecentFromStore(t *testing.T) {
	rec := &fakeRec{today: store.Totals{Usage: store.Usage{GenTokens: 7}, HasData: true}}
	for i := 5; i >= 1; i-- {
		rec.recent = append(rec.recent, stats.Finished{TaskID: i, At: t0.Add(-time.Duration(10-i) * time.Minute)})
	}
	srv := fakeserver.New(busyIdleSteps(1))
	defer srv.Close()
	c := newC(srv.URL, func(c *Config) { c.Store, c.KWhPrice, c.Currency = rec, 0.3, "GBP" })
	if !c.Start(t0, time.Second) {
		t.Fatal("Start timed out")
	}
	if len(rec.tariffs) != 1 || rec.tariffs[0] != 0.3 {
		t.Fatalf("tariffs %v", rec.tariffs)
	}
	s := c.Poll(context.Background(), t0)
	if len(s.Recent) != 5 || s.Recent[0].TaskID != 5 || s.Recent[4].TaskID != 1 {
		t.Fatalf("recent %+v", s.Recent)
	}
	if !s.Today.HasData || s.Today.GenTokens != 7 || !s.AllTime.HasData {
		t.Fatalf("totals not seeded: %+v", s.Today)
	}
	s = c.Poll(context.Background(), t0.Add(time.Second))
	if len(s.Recent) != 6 || s.Recent[0].TaskID != 101 || s.Recent[1].TaskID != 5 {
		t.Fatalf("after a finish: %+v", s.Recent)
	}
}

func TestStartWithoutPriceSetsNoTariff(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	c.Start(t0, time.Second)
	if len(rec.tariffs) != 0 {
		t.Fatalf("tariffs %v", rec.tariffs)
	}
}

func TestSuspendGap(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, constPower(300, 1000)
		c.Store, c.KWhPrice = rec, 0.3
	})
	for i := 0; i < 5; i++ {
		pollGPU(t, c, t0.Add(time.Duration(i)*time.Second))
	}
	resume := t0.Add(2 * time.Hour)
	var s Snapshot
	for i := 0; i <= 10; i++ {
		s = pollGPU(t, c, resume.Add(time.Duration(i)*time.Second))
	}
	if len(rec.samples) != 2 || !rec.samples[0].At.Equal(t0) || !rec.samples[1].At.Equal(resume) {
		t.Fatalf("samples %+v", rec.samples)
	}
	if len(rec.usage) != 2 || !rec.usage[0].at.Equal(t0.Add(4*time.Second)) || !rec.usage[1].at.Equal(resume.Add(10*time.Second)) {
		t.Fatalf("usage %+v", rec.usage)
	}
	u := rec.sumUsage()
	// GPU samples every 2s: 0->2->4 before the gap, resume+0 -> +8 after it; nothing for the gap
	if !near(u.GPUEnergyWh, 300*12.0/3600, 1e-6) || !near(s.GPUEnergyWh, u.GPUEnergyWh, 1e-9) {
		t.Fatalf("usage %+v, snapshot %v", u, s.GPUEnergyWh)
	}
}

func TestMidnightSplit(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	start := time.Date(2026, 11, 24, 23, 59, 48, 0, time.Local)
	for i := 0; i <= 20; i++ {
		c.Poll(context.Background(), start.Add(time.Duration(i)*time.Second))
	}
	if len(rec.usage) != 2 ||
		!rec.usage[0].at.Equal(time.Date(2026, 11, 24, 23, 59, 58, 0, time.Local)) ||
		!rec.usage[1].at.Equal(time.Date(2026, 11, 25, 0, 0, 8, 0, time.Local)) {
		t.Fatalf("usage %+v", rec.usage)
	}
}

func TestStoreNeverBlocksPoll(t *testing.T) {
	rec := &fakeRec{totalsGate: make(chan struct{})}
	rec.today = store.Totals{HasData: true}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	for i := 0; i < 40; i++ {
		start := time.Now()
		s := c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
		if d := time.Since(start); d > c.timeout+500*time.Millisecond {
			t.Fatalf("Poll %d took %v", i, d)
		}
		if s.Today.HasData {
			t.Fatal("totals set while the read is in flight")
		}
	}
	if n := atomic.LoadInt32(&rec.totalsCalls); n != 1 {
		t.Fatalf("%d Totals calls, want one in flight", n)
	}
	close(rec.totalsGate)
	for i := 0; i < 400; i++ {
		if s := c.Poll(context.Background(), t0.Add(41*time.Second)); s.Today.HasData {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("totals never picked up")
}

func TestStartBounded(t *testing.T) {
	rec := &fakeRec{totalsGate: make(chan struct{})}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	start := time.Now()
	if c.Start(t0, 30*time.Millisecond) {
		t.Fatal("Start reported success while Totals blocks")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Start took %v", d)
	}
	if s := c.Poll(context.Background(), t0); s.DBStatus != "off (locked)" {
		t.Fatalf("DBStatus %q", s.DBStatus)
	}
	close(rec.totalsGate)
	for i := 0; i < 400; i++ {
		if s := c.Poll(context.Background(), t0.Add(time.Second)); s.DBStatus == "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("DBStatus never cleared")
}

func TestDBStatus(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	rec.setErr(fmt.Errorf("store: write: %w", lockedErr{}))
	if s := c.Poll(context.Background(), t0); s.DBStatus != "off (locked)" {
		t.Fatalf("DBStatus %q", s.DBStatus)
	}
	rec.setErr(nil)
	if s := c.Poll(context.Background(), t0.Add(time.Second)); s.DBStatus != "" {
		t.Fatalf("DBStatus %q after recovery", s.DBStatus)
	}

	c = newC("http://127.0.0.1:1", func(c *Config) { c.DBOpenErr = fmt.Errorf("store: x: %w", store.ErrNotDatabase) })
	if s := c.Poll(context.Background(), t0); s.DBStatus != "off (not a database)" {
		t.Fatalf("open failure: DBStatus %q", s.DBStatus)
	}
}

func TestNoStore(t *testing.T) {
	srv := fakeserver.New(busyIdleSteps(2))
	defer srv.Close()
	c := newC(srv.URL, nil)
	if !c.Start(t0, time.Second) {
		t.Fatal("Start without a store must succeed at once")
	}
	var s Snapshot
	for i := 0; i <= 25; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if s.DBStatus != "" || s.Today.HasData || s.Month.HasData || s.AllTime.HasData || len(s.Recent) != 2 {
		t.Fatalf("%q %+v %+v", s.DBStatus, s.Today, s.Recent)
	}
}

func TestPruneErrInDBStatus(t *testing.T) {
	rec := &fakeRec{}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.Store = rec })
	c.SetPruneErr(fmt.Errorf("store: prune: %w", lockedErr{}))
	if s := c.Poll(context.Background(), t0); s.DBStatus != "off (locked)" {
		t.Fatalf("DBStatus %q", s.DBStatus)
	}
	c.SetPruneErr(nil)
	if s := c.Poll(context.Background(), t0.Add(time.Second)); s.DBStatus != "" {
		t.Fatalf("DBStatus %q after a good prune", s.DBStatus)
	}
}

func TestFlushHistory(t *testing.T) {
	srv := fakeserver.New(busyIdleSteps(1))
	defer srv.Close()
	rec := &fakeRec{}
	c := newC(srv.URL, func(c *Config) { c.Store, c.KWhPrice = rec, 0.3 })
	for i := 0; i < 3; i++ { // busy, idle (the request finishes), idle: all in the first window
		c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(rec.samples) != 0 || len(rec.usage) != 0 || len(rec.requests) != 0 {
		t.Fatalf("sent before the window closed: %+v %+v", rec.samples, rec.usage)
	}
	end := t0.Add(3 * time.Second)
	c.FlushHistory(end)
	c.FlushHistory(end.Add(time.Second))
	if len(rec.samples) != 1 || !rec.samples[0].At.Equal(t0) {
		t.Fatalf("samples %+v", rec.samples)
	}
	if len(rec.requests) != 1 || rec.requests[0].TaskID != 101 {
		t.Fatalf("requests %+v", rec.requests)
	}
	if len(rec.usage) != 1 || !rec.usage[0].at.Equal(end) || rec.usage[0].u.Requests != 1 {
		t.Fatalf("usage %+v", rec.usage)
	}

	newC("http://127.0.0.1:1", nil).FlushHistory(end) // no store: no panic
	empty := &fakeRec{}
	newC("http://127.0.0.1:1", func(c *Config) { c.Store = empty }).FlushHistory(end)
	if len(empty.samples)+len(empty.usage) != 0 {
		t.Fatalf("no window, yet sent %+v %+v", empty.samples, empty.usage)
	}
}
