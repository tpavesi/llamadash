package collect

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llamadash/internal/container"
	"llamadash/internal/cpupower"
	"llamadash/internal/fakeserver"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/hwmon"
	"llamadash/internal/procstat"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const propsOn = `{"total_slots":1,"model_alias":"m","build_info":"b1","endpoint_slots":true,"endpoint_metrics":true,"default_generation_settings":{"n_ctx":4096}}`
const propsOff = `{"total_slots":1,"model_alias":"m","build_info":"b1","endpoint_slots":true,"endpoint_metrics":false,"default_generation_settings":{"n_ctx":4096}}`

func slotsBody(decoded int) []byte {
	return []byte(fmt.Sprintf(`[{"id":0,"n_ctx":4096,"is_processing":true,"id_task":1,"n_prompt_tokens":100,"n_prompt_tokens_processed":10,"n_prompt_tokens_cache":90,"next_token":[{"n_decoded":%d,"n_remain":-1}]}]`, decoded))
}

func step(props string, slots fakeserver.Response, metrics fakeserver.Response) fakeserver.Step {
	return fakeserver.Step{
		Health:  fakeserver.Response{Status: 200, Body: []byte(`{"status":"ok"}`)},
		Props:   fakeserver.Response{Status: 200, Body: []byte(props)},
		Slots:   slots,
		Metrics: metrics,
	}
}

func ok(b []byte) fakeserver.Response { return fakeserver.Response{Status: 200, Body: b} }

func newC(url string, mut func(*Config)) *Collector {
	// default CPU source is unavailable so tests never read the real /sys
	cfg := Config{URL: url, Interval: time.Second, GPU: GPUOff, CPUPower: cpuStub(nil, 0, 0, false, cpupower.ErrUnavailable),
		Host: hostStub(nil, host.Info{}, host.ErrUnavailable), Proc: procStub(nil, procstat.Info{}, procstat.ErrUnavailable),
		Hwmon:     hwmonStub(nil, hwmon.Reading{}, hwmon.ErrUnavailable),
		Container: func(context.Context, int) (container.Info, error) { return container.Info{}, container.ErrUnavailable }}
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg)
}

func TestPollHappyPath(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	c := newC(srv.URL, nil)
	var s Snapshot
	for i := 0; i < 2; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if !s.Reachable || s.Props == nil || s.Props.ModelAlias != "example-model-27b" {
		t.Fatalf("snapshot: %+v", s)
	}
	if len(s.Slots) != 1 || s.GenRate < 0 || len(s.GenHist) != 2 {
		t.Fatalf("slots=%d gen=%v hist=%d", len(s.Slots), s.GenRate, len(s.GenHist))
	}
	if s.Health.Status != "ok" || s.SlotsAt.IsZero() || !s.At.Equal(t0.Add(time.Second)) {
		t.Fatalf("health/slotsAt/at: %+v", s)
	}
}

func TestMetricsDisabledHint(t *testing.T) {
	// Props say metrics on, but /metrics answers 501.
	srv := fakeserver.NewCounting([]fakeserver.Step{step(propsOn, ok(slotsBody(0)), fakeserver.Response{Status: 501, Body: []byte(`{}`)})})
	defer srv.Close()
	c := newC(srv.URL, nil)
	s := c.Poll(context.Background(), t0)
	if s.Metrics != nil || !strings.Contains(s.MetricsHint, "--metrics") {
		t.Fatalf("501: %+v", s)
	}

	// Props say metrics off: hint, and /metrics is not polled again.
	srv2 := fakeserver.NewCounting([]fakeserver.Step{step(propsOff, ok(slotsBody(0)), ok([]byte("llamacpp:requests_deferred 1\n")))})
	defer srv2.Close()
	c = newC(srv2.URL, nil)
	c.Poll(context.Background(), t0)
	first := srv2.Hits("/metrics")
	c.Poll(context.Background(), t0.Add(time.Second))
	s = c.Poll(context.Background(), t0.Add(2*time.Second))
	if s.Metrics != nil || !strings.Contains(s.MetricsHint, "--metrics") {
		t.Fatalf("props off: %+v", s)
	}
	if srv2.Hits("/metrics") != first {
		t.Fatalf("/metrics polled after props said disabled: %d then %d", first, srv2.Hits("/metrics"))
	}
}

func TestMetricsPresent(t *testing.T) {
	srv := fakeserver.New([]fakeserver.Step{step(propsOn, ok(slotsBody(0)), ok([]byte("llamacpp:requests_deferred 3\n")))})
	defer srv.Close()
	s := newC(srv.URL, nil).Poll(context.Background(), t0)
	if s.Metrics["requests_deferred"] != 3 || s.MetricsHint != "" {
		t.Fatalf("%+v", s)
	}
}

func hintFor(slots fakeserver.Response) Snapshot {
	srv := fakeserver.New([]fakeserver.Step{step(propsOn, slots, fakeserver.Response{Status: 501})})
	defer srv.Close()
	return newC(srv.URL, nil).Poll(context.Background(), t0)
}

func TestSlotsDisabledHint(t *testing.T) {
	s := hintFor(fakeserver.Response{Status: 501})
	if !strings.Contains(s.SlotsHint, "--slots") || len(s.Slots) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestSlotsUnauthorizedHint(t *testing.T) {
	s := hintFor(fakeserver.Response{Status: 401})
	if !strings.Contains(s.SlotsHint, "--api-key") {
		t.Fatalf("%+v", s)
	}
}

func TestSlotsBadShapeHint(t *testing.T) {
	s := hintFor(ok([]byte(`{"x":1}`)))
	if !strings.Contains(s.SlotsHint, "unrecognised") || !strings.Contains(s.SlotsHint, "b1") {
		t.Fatalf("%+v", s)
	}
}

func TestOutageAndRecovery(t *testing.T) {
	srv := fakeserver.NewSwitch([]fakeserver.Step{step(propsOn, ok(slotsBody(100)), ok([]byte("")))})
	defer srv.Close()
	c := newC(srv.URL, nil)
	c.Poll(context.Background(), t0)
	s := c.Poll(context.Background(), t0.Add(time.Second))
	if !s.Reachable || len(s.Slots) != 1 {
		t.Fatalf("before outage: %+v", s)
	}
	srv.Refuse()

	var first Snapshot
	for i := 0; i < 3; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(2+i)*time.Second))
		if i == 0 {
			first = s
		}
	}
	if s.Reachable || s.UnreachableSince.IsZero() || !s.UnreachableSince.Equal(first.UnreachableSince) ||
		!s.UnreachableSince.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("outage: %+v", s)
	}
	if len(s.Slots) != 1 || s.Props == nil {
		t.Fatalf("slots/props not retained: %+v", s)
	}

	loading := step(propsOn, ok(slotsBody(100)), ok(nil))
	loading.Health = fakeserver.Response{Status: 503, Body: []byte(`{"error":{"message":"Loading model"}}`)}
	loading.Slots = fakeserver.Response{Status: 503}
	loading.Props = fakeserver.Response{Status: 503}
	healthy := step(propsOn, ok(slotsBody(900)), ok(nil))
	srv.Serve([]fakeserver.Step{loading, healthy})
	s = c.Poll(context.Background(), t0.Add(5*time.Second))
	if !s.Reachable || !s.Loading || s.UnreachableSince != (time.Time{}) {
		t.Fatalf("loading: %+v", s)
	}
	// Every endpoint advances one step per hit, so the second poll sees the healthy step.
	s = c.Poll(context.Background(), t0.Add(6*time.Second))
	if !s.Reachable || s.Loading || !s.UnreachableSince.IsZero() || s.Props == nil {
		t.Fatalf("recovered: %+v", s)
	}
	if srv.Hits("/props") < 2 {
		t.Fatalf("props not re-fetched until success: %d", srv.Hits("/props"))
	}
	if s.GenRate != 0 || len(s.Slots) != 1 || s.Slots[0].Decoded != 900 {
		t.Fatalf("first rate after recovery should be 0: %+v", s)
	}
}

func TestHistoryBounded(t *testing.T) {
	srv := fakeserver.New([]fakeserver.Step{step(propsOff, ok(slotsBody(1)), fakeserver.Response{Status: 501})})
	defer srv.Close()
	c := newC(srv.URL, nil)
	var s Snapshot
	for i := 0; i < 400; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(s.GenHist) != 300 || len(s.PromptHist) != 300 {
		t.Fatalf("hist %d/%d", len(s.GenHist), len(s.PromptHist))
	}
}

func gpuStub(calls *int32, gpus []gpu.GPU, err error) func(context.Context) ([]gpu.GPU, error) {
	return func(context.Context) ([]gpu.GPU, error) {
		atomic.AddInt32(calls, 1)
		return gpus, err
	}
}

// pollGPU polls and then waits for the GPU query that poll may have started. GPU
// results arrive asynchronously: the next Poll is the first to see them.
func pollGPU(t *testing.T, c *Collector, at time.Time) Snapshot {
	t.Helper()
	s := c.Poll(context.Background(), at)
	if !c.WaitGPU(time.Second) {
		t.Fatal("GPU query did not return")
	}
	return s
}

func TestGPUAutoLoopbackOnly(t *testing.T) {
	var calls int32
	q := gpuStub(&calls, []gpu.GPU{{Index: 0}}, nil)
	mut := func(c *Config) { c.GPU, c.GPUQuery = GPUAuto, q }
	pollGPU(t, newC("http://192.0.2.10:8011", mut), t0)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("remote host must not query GPU in auto mode")
	}
	for _, u := range []string{"http://127.0.0.1:1", "http://localhost:1", "http://[::1]:1", "http://127.5.5.5:1"} {
		atomic.StoreInt32(&calls, 0)
		pollGPU(t, newC(u, mut), t0)
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Fatalf("%s: calls=%d", u, n)
		}
	}
	atomic.StoreInt32(&calls, 0)
	pollGPU(t, newC("http://192.0.2.10:8011", func(c *Config) { c.GPU, c.GPUQuery = GPUOn, q }), t0)
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatal("GPUOn must always query")
	}
	atomic.StoreInt32(&calls, 0)
	pollGPU(t, newC("http://127.0.0.1:1", func(c *Config) { c.GPU, c.GPUQuery = GPUOff, q }), t0)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("GPUOff must never query")
	}
}

func TestGPUResultArrivesNextPoll(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), []gpu.GPU{{Index: 0, Name: "x"}}, nil)
	})
	if s := pollGPU(t, c, t0); s.GPUs != nil {
		t.Fatalf("first poll cannot have GPU data yet: %+v", s.GPUs)
	}
	s := pollGPU(t, c, t0.Add(time.Second))
	if len(s.GPUs) != 1 || !s.GPUAt.Equal(t0) {
		t.Fatalf("second poll: %+v at %v", s.GPUs, s.GPUAt)
	}
}

func TestGPUUnavailableStopsQuerying(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(&calls, nil, gpu.ErrUnavailable)
	})
	var s Snapshot
	for i := 0; i < 10; i++ {
		s = pollGPU(t, c, t0.Add(time.Duration(i)*3*time.Second))
	}
	if n := atomic.LoadInt32(&calls); n != 1 || s.GPUs != nil {
		t.Fatalf("calls=%d gpus=%v", n, s.GPUs)
	}
}

func TestGPUOtherErrorKeepsLastValue(t *testing.T) {
	var n int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUOn
		c.GPUQuery = func(context.Context) ([]gpu.GPU, error) {
			if atomic.AddInt32(&n, 1) == 1 {
				return []gpu.GPU{{Index: 0, Name: "x"}}, nil
			}
			return nil, errors.New("boom")
		}
	})
	pollGPU(t, c, t0)
	pollGPU(t, c, t0.Add(3*time.Second)) // takes the good result, starts the failing one
	s := pollGPU(t, c, t0.Add(6*time.Second))
	if len(s.GPUs) != 1 || !s.GPUAt.Equal(t0) {
		t.Fatalf("%+v", s)
	}
}

func TestGPUThrottled(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.Interval = 250 * time.Millisecond
		c.GPU, c.GPUQuery = GPUOn, gpuStub(&calls, []gpu.GPU{{}}, nil)
	})
	for i := 0; i <= 8; i++ {
		pollGPU(t, c, t0.Add(time.Duration(i)*250*time.Millisecond))
	}
	if n := atomic.LoadInt32(&calls); n > 2 || n < 1 {
		t.Fatalf("calls=%d", n)
	}
}

// powerSeq returns a GPUQuery that yields the given per-call GPU lists.
func powerSeq(seq ...[]gpu.GPU) func(context.Context) ([]gpu.GPU, error) {
	var i int32
	return func(context.Context) ([]gpu.GPU, error) {
		n := int(atomic.AddInt32(&i, 1)) - 1
		return seq[min(n, len(seq)-1)], nil
	}
}

func pw(w float64) []gpu.GPU { return []gpu.GPU{{PowerW: w, HasPower: true}} }

func energyCollector(q func(context.Context) ([]gpu.GPU, error)) *Collector {
	return newC("http://127.0.0.1:1", func(c *Config) { c.GPU, c.GPUQuery = GPUOn, q })
}

func TestEnergyIntegration(t *testing.T) {
	c := energyCollector(powerSeq(pw(300), pw(300), pw(100)))
	var s Snapshot
	for i := 0; i < 4; i++ { // the samples of polls 0..2 are taken by polls 1..3
		s = pollGPU(t, c, t0.Add(time.Duration(i)*2*time.Second))
	}
	if math.Abs(s.EnergyWh-0.2778) > 0.001 || s.PowerW != 100 || len(s.PowerHist) != 3 || !s.HasPower {
		t.Fatalf("%+v", s)
	}
}

func TestEnergySkipsLongGap(t *testing.T) {
	c := energyCollector(powerSeq(pw(300), pw(300)))
	pollGPU(t, c, t0)
	pollGPU(t, c, t0.Add(time.Second)) // takes the sample of t0
	pollGPU(t, c, t0.Add(60*time.Second))
	s := pollGPU(t, c, t0.Add(61*time.Second)) // takes the sample of t0+60s
	if s.EnergyWh != 0 || len(s.PowerHist) != 2 {
		t.Fatalf("%+v", s)
	}
}

// At --interval 10s the real gap is the interval plus the poll time, about 10.05s:
// rates and energy must still accumulate.
func TestTenSecondCadence(t *testing.T) {
	srv := fakeserver.New([]fakeserver.Step{
		step(propsOff, ok(slotsBody(100)), fakeserver.Response{Status: 501}),
		step(propsOff, ok(slotsBody(700)), fakeserver.Response{Status: 501}),
		step(propsOff, ok(slotsBody(1300)), fakeserver.Response{Status: 501}),
	})
	defer srv.Close()
	c := newC(srv.URL, func(c *Config) {
		c.Interval = 10 * time.Second
		c.GPU, c.GPUQuery = GPUOn, powerSeq(pw(300))
	})
	const dt = 10050 * time.Millisecond
	var s [4]Snapshot
	for i := range s { // the GPU sample of poll i is taken by poll i+1
		s[i] = pollGPU(t, c, t0.Add(time.Duration(i)*dt))
	}
	if want := 600 / dt.Seconds(); math.Abs(s[2].GenRate-want) > 1e-6 {
		t.Errorf("GenRate %v want %v", s[2].GenRate, want)
	}
	if want := 300 * 2 * dt.Seconds() / 3600; math.Abs(s[3].EnergyWh-want) > 1e-6 {
		t.Errorf("EnergyWh %v want %v", s[3].EnergyWh, want)
	}
}

func TestEnergyTwoGPUsAndNoPower(t *testing.T) {
	two := []gpu.GPU{{PowerW: 200, HasPower: true}, {PowerW: 150, HasPower: true}, {PowerW: 99}}
	c := energyCollector(powerSeq(two))
	pollGPU(t, c, t0)
	s := pollGPU(t, c, t0.Add(time.Second))
	if s.PowerW != 350 || !s.HasPower || !s.HasGPUPower {
		t.Fatalf("%+v", s)
	}
	none := []gpu.GPU{{PowerW: 50}, {}}
	c = energyCollector(powerSeq(none))
	for i := 0; i < 3; i++ {
		s = pollGPU(t, c, t0.Add(time.Duration(i)*2*time.Second))
	}
	if s.HasPower || s.HasGPUPower || s.EnergyWh != 0 || s.PowerW != 0 || len(s.PowerHist) != 0 {
		t.Fatalf("%+v", s)
	}
}

// GPU power leaves the totals once the GPU query has been failing for over 30s;
// CPU power carries on alone.
func TestGPUPowerExpires(t *testing.T) {
	var n int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUOn
		c.GPUQuery = func(context.Context) ([]gpu.GPU, error) {
			if atomic.AddInt32(&n, 1) == 1 {
				return pw(300), nil
			}
			return nil, errors.New("nvidia-smi failed")
		}
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
	})
	pollGPU(t, c, t0)
	s := pollGPU(t, c, t0.Add(2*time.Second))
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("before: %+v", s)
	}
	for i := 2; i <= 15; i++ { // up to 30s after the good reading: still shown
		s = pollGPU(t, c, t0.Add(time.Duration(i)*2*time.Second))
	}
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("at 30s: %+v", s)
	}
	s = pollGPU(t, c, t0.Add(32*time.Second))
	if s.HasGPUPower || s.GPUPowerW != 0 || s.PowerW != 100 || !s.HasPower ||
		s.JoulesPerToken != 0 || s.PowerHist[len(s.PowerHist)-1] != 100 {
		t.Fatalf("after 30s: %+v", s)
	}
}

func TestEnergyMonotonicAcrossOutage(t *testing.T) {
	srv := fakeserver.New([]fakeserver.Step{step(propsOff, ok(slotsBody(0)), fakeserver.Response{Status: 501})})
	c := newC(srv.URL, func(c *Config) { c.GPU, c.GPUQuery = GPUOn, powerSeq(pw(360)) })
	pollGPU(t, c, t0)
	pollGPU(t, c, t0.Add(2*time.Second))
	s := pollGPU(t, c, t0.Add(4*time.Second))
	srv.Close()
	s2 := pollGPU(t, c, t0.Add(6*time.Second))
	if s.EnergyWh <= 0 || s2.EnergyWh < s.EnergyWh || s2.Reachable {
		t.Fatalf("%v then %v", s.EnergyWh, s2.EnergyWh)
	}
}

func TestJoulesPerToken(t *testing.T) {
	for _, tc := range []struct {
		d2   int
		dt   time.Duration
		want float64
	}{{140, time.Second, 7.5}, {101, 5 * time.Second, 0}} {
		srv := fakeserver.New([]fakeserver.Step{
			step(propsOff, ok(slotsBody(100)), fakeserver.Response{Status: 501}),
			step(propsOff, ok(slotsBody(tc.d2)), fakeserver.Response{Status: 501}),
		})
		c := newC(srv.URL, func(c *Config) { c.GPU, c.GPUQuery = GPUOn, powerSeq(pw(300)) })
		pollGPU(t, c, t0)
		s := pollGPU(t, c, t0.Add(tc.dt))
		srv.Close()
		if math.Abs(s.JoulesPerToken-tc.want) > 1e-9 {
			t.Fatalf("decoded %d: gen=%v J/token=%v want %v", tc.d2, s.GenRate, s.JoulesPerToken, tc.want)
		}
	}
}

func TestGPUHangDoesNotBlock(t *testing.T) {
	srv := fakeserver.New([]fakeserver.Step{step(propsOn, ok(slotsBody(0)), fakeserver.Response{Status: 501})})
	defer srv.Close()
	c := newC(srv.URL, func(c *Config) {
		c.Interval = 250 * time.Millisecond // timeout 1s
		c.GPU = GPUOn
		c.GPUQuery = func(ctx context.Context) ([]gpu.GPU, error) { <-ctx.Done(); return nil, ctx.Err() }
	})
	start := time.Now()
	s := c.Poll(context.Background(), t0)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Poll took %v", d)
	}
	if !s.Reachable || s.Props == nil || len(s.Slots) != 1 {
		t.Fatalf("%+v", s)
	}
}

// A query that ignores its context (a hung nvidia-smi) must not be abandoned and
// started again on every due poll.
func TestGPUHungQueryStartedOnce(t *testing.T) {
	var calls int32
	release, started := make(chan struct{}), make(chan struct{}, 10)
	t.Cleanup(func() { close(release) })
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUOn
		c.GPUQuery = func(context.Context) ([]gpu.GPU, error) {
			atomic.AddInt32(&calls, 1)
			started <- struct{}{}
			<-release // ignores ctx
			return nil, errors.New("released")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	for i := 0; i < 5; i++ { // every poll is due (3s apart)
		c.Poll(ctx, t0.Add(time.Duration(i)*3*time.Second))
	}
	select { // the query runs in the background: make sure it has started
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("query never started")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("hung query started %d times over 5 due polls, want 1", n)
	}
}

// Same guard for the CPU power source.
func TestCPUHungReadStartedOnce(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), nil, gpu.ErrUnavailable)
		c.CPUPower = func(time.Time) (float64, float64, bool, error) {
			atomic.AddInt32(&calls, 1)
			<-release
			return 0, 0, false, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	for i := 0; i < 5; i++ {
		c.Poll(ctx, t0.Add(time.Duration(i)*time.Second))
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("hung CPU read started %d times over 5 polls, want 1", n)
	}
}

func TestTimeout(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		time.Second: time.Second, 250 * time.Millisecond: time.Second, 500 * time.Millisecond: time.Second,
		1500 * time.Millisecond: 1200 * time.Millisecond, 2 * time.Second: 1600 * time.Millisecond,
		10 * time.Second: 1600 * time.Millisecond,
	} {
		if got := timeoutFor(in); got != want {
			t.Errorf("%v: %v want %v", in, got, want)
		}
	}
}

func TestNoStaleRatesAfterOutage(t *testing.T) {
	srv := fakeserver.NewSwitch([]fakeserver.Step{
		step(propsOff, ok(slotsBody(100)), fakeserver.Response{Status: 501}),
		step(propsOff, ok(slotsBody(140)), fakeserver.Response{Status: 501}),
	})
	defer srv.Close()
	c := newC(srv.URL, nil)
	c.Poll(context.Background(), t0)
	s := c.Poll(context.Background(), t0.Add(time.Second))
	if s.GenRate != 40 {
		t.Fatalf("precondition: gen=%v", s.GenRate)
	}
	srv.Refuse()
	if s = c.Poll(context.Background(), t0.Add(2*time.Second)); s.Reachable {
		t.Fatalf("refusing server reported reachable: %+v", s)
	}

	srv.Serve([]fakeserver.Step{step(propsOff, fakeserver.Response{Status: 503}, fakeserver.Response{Status: 501})})
	s = c.Poll(context.Background(), t0.Add(3*time.Second))
	if !s.Reachable || s.GenRate != 0 || s.PromptRate != 0 || s.GenHist[len(s.GenHist)-1] != 0 {
		t.Fatalf("stale rate reported live: gen=%v hist=%v", s.GenRate, s.GenHist)
	}
	for _, v := range s.Slots {
		if v.GenRate != 0 || v.PromptRate != 0 {
			t.Fatalf("retained view keeps live rate: %+v", v)
		}
	}
}

func cpuStub(calls *int32, w, j float64, ok bool, err error) func(time.Time) (float64, float64, bool, error) {
	return func(time.Time) (float64, float64, bool, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return w, j, ok, err
	}
}

func TestCPUPowerTotals(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, powerSeq(pw(300))
		c.CPUPower = cpuStub(nil, 100, 200, true, nil)
	})
	var s Snapshot
	for i := 0; i < 4; i++ { // GPU samples of polls 0..2 are taken by polls 1..3; CPU reads every poll
		s = pollGPU(t, c, t0.Add(time.Duration(i)*2*time.Second))
	}
	if !s.HasPower || !s.HasCPUPower || s.PowerW != 400 || s.GPUPowerW != 300 || s.CPUPowerW != 100 {
		t.Fatalf("%+v", s)
	}
	if math.Abs(s.CPUEnergyWh-800.0/3600) > 1e-9 || math.Abs(s.GPUEnergyWh-1200.0/3600) > 1e-9 ||
		math.Abs(s.EnergyWh-(s.GPUEnergyWh+s.CPUEnergyWh)) > 1e-12 {
		t.Fatalf("cpu=%v gpu=%v total=%v", s.CPUEnergyWh, s.GPUEnergyWh, s.EnergyWh)
	}
	if len(s.PowerHist) != 4 || s.PowerHist[0] != 100 || s.PowerHist[3] != 400 {
		t.Fatalf("hist %v", s.PowerHist)
	}
}

func TestCPUPowerOnlyNoGPU(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), nil, gpu.ErrUnavailable)
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
	})
	s := c.Poll(context.Background(), t0)
	if !s.HasPower || s.PowerW != 100 || s.GPUs != nil || s.GPUPowerW != 0 || s.CPUPowerW != 100 {
		t.Fatalf("%+v", s)
	}
}

func TestCPUPowerLoopbackOnly(t *testing.T) {
	var calls int32
	cpu := func(c *Config) { c.CPUPower = cpuStub(&calls, 100, 100, true, nil) }
	newC("http://192.0.2.10:8011", func(c *Config) { cpu(c); c.GPU = GPUAuto }).Poll(context.Background(), t0)
	newC("http://127.0.0.1:1", func(c *Config) { cpu(c); c.GPU = GPUOff }).Poll(context.Background(), t0)
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
	newC("http://127.0.0.1:1", func(c *Config) { cpu(c); c.GPU = GPUAuto; c.GPUQuery = gpuStub(new(int32), nil, gpu.ErrUnavailable) }).Poll(context.Background(), t0)
	newC("http://192.0.2.10:8011", func(c *Config) { cpu(c); c.GPU = GPUOn; c.GPUQuery = gpuStub(new(int32), nil, gpu.ErrUnavailable) }).Poll(context.Background(), t0)
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestCPUUnavailableStops(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), nil, gpu.ErrUnavailable)
		c.CPUPower = cpuStub(&calls, 0, 0, false, cpupower.ErrUnavailable)
	})
	var s Snapshot
	for i := 0; i < 10; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if calls != 1 || s.HasCPUPower || s.HasPower {
		t.Fatalf("calls=%d %+v", calls, s)
	}
}

func TestCPUFirstSampleNotOK(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), nil, gpu.ErrUnavailable)
		c.CPUPower = cpuStub(nil, 0, 0, false, nil)
	})
	if s := c.Poll(context.Background(), t0); s.HasCPUPower || s.HasPower || len(s.PowerHist) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestSetIntervalShortensTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hang until the test ends or the client gives up
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := newC(srv.URL, func(cfg *Config) { cfg.Interval = 10 * time.Second }) // timeout 1.6s
	c.SetInterval(250 * time.Millisecond)                                     // timeout 1s
	start := time.Now()
	s := c.Poll(context.Background(), t0)
	if d := time.Since(start); d > 1400*time.Millisecond {
		t.Fatalf("poll took %v after SetInterval(250ms)", d)
	}
	if s.Reachable {
		t.Fatalf("hanging server reported reachable")
	}
	c.SetInterval(time.Hour) // clamped to 10s
	if c.timeout != 1600*time.Millisecond {
		t.Fatalf("clamp: timeout %v", c.timeout)
	}
	c.SetInterval(time.Millisecond) // clamped to 250ms
	if c.timeout != time.Second || c.cfg.Interval != 250*time.Millisecond {
		t.Fatalf("clamp: timeout %v interval %v", c.timeout, c.cfg.Interval)
	}
	client := c.client
	c.SetInterval(250 * time.Millisecond)
	if c.client != client {
		t.Fatal("SetInterval with the unchanged interval rebuilt the client")
	}
}

func TestHealthErrorStatus(t *testing.T) {
	for name, h := range map[string]fakeserver.Response{
		"500": {Status: 500, Body: []byte(`{}`)}, "not json": {Status: 200, Body: []byte(`<html>`)},
	} {
		st := step(propsOn, ok(slotsBody(0)), fakeserver.Response{Status: 501})
		st.Health = h
		srv := fakeserver.New([]fakeserver.Step{st})
		s := newC(srv.URL, nil).Poll(context.Background(), t0)
		srv.Close()
		if !s.Reachable || s.Health.Status != "error" || s.Loading {
			t.Errorf("%s: reachable=%v status=%q loading=%v", name, s.Reachable, s.Health.Status, s.Loading)
		}
	}
}

func TestSlotsHTTPErrorHint(t *testing.T) {
	s := hintFor(fakeserver.Response{Status: 500, Body: []byte(`{"error":"x"}`)})
	if s.SlotsHint != "HTTP error from /slots" || len(s.Slots) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestRecentNewestFirstAndCapped(t *testing.T) {
	busy := func(task int) []byte {
		return []byte(fmt.Sprintf(`[{"id":0,"n_ctx":4096,"is_processing":true,"id_task":%d,"n_prompt_tokens":100,"n_prompt_tokens_processed":10,"n_prompt_tokens_cache":90,"next_token":[{"n_decoded":5,"n_remain":-1}]}]`, task))
	}
	idle := func(task int) []byte {
		return []byte(fmt.Sprintf(`[{"id":0,"n_ctx":4096,"is_processing":false,"id_task":%d,"n_prompt_tokens":105,"n_prompt_tokens_processed":0,"n_prompt_tokens_cache":0,"next_token":[{"n_decoded":0,"n_remain":-1}]}]`, task))
	}
	var steps []fakeserver.Step
	for i := 1; i <= 60; i++ {
		steps = append(steps, step(propsOff, ok(busy(i)), ok(nil)), step(propsOff, ok(idle(i)), ok(nil)))
	}
	srv := fakeserver.NewCounting(steps)
	defer srv.Close()
	c := newC(srv.URL, nil)
	var s Snapshot
	for i := 0; i < len(steps); i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(s.Recent) != 50 || s.Recent[0].TaskID != 60 || s.Recent[49].TaskID != 11 {
		t.Fatalf("len %d, snapshot %+v", len(s.Recent), s.Recent)
	}
	s.Recent[0].TaskID = -1 // the snapshot must not alias collector state
	if again := c.Poll(context.Background(), t0.Add(200*time.Second)); again.Recent[0].TaskID != 60 {
		t.Fatalf("snapshot aliases collector state: %+v", again.Recent[0])
	}
}

// After a pause longer than the GPU power age (a suspend, a stopped terminal) the
// first poll takes a result that is one poll old: it must not be expired against
// the poll time, or total power drops the GPU for a frame and PowerHist dips.
func TestGPUPowerSurvivesPause(t *testing.T) {
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), pw(300), nil)
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
	})
	pollGPU(t, c, t0)
	pollGPU(t, c, t0.Add(2*time.Second))       // takes the first result, starts the second
	s := pollGPU(t, c, t0.Add(62*time.Second)) // 60s later: takes a result started before the pause
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("after pause: HasGPUPower=%v PowerW=%v", s.HasGPUPower, s.PowerW)
	}
	for i, v := range s.PowerHist[1:] { // [0] is the CPU-only sample before any GPU result
		if v != 400 {
			t.Fatalf("PowerHist[%d]=%v, want 400 (dip): %v", i+1, v, s.PowerHist)
		}
	}
}

func hostStub(calls *int32, in host.Info, err error) func() (host.Info, error) {
	return func() (host.Info, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return in, err
	}
}

func procStub(calls *int32, in procstat.Info, err error) func(time.Time) (procstat.Info, error) {
	return func(time.Time) (procstat.Info, error) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return in, err
	}
}

func TestLocalSensorsLoopbackOnly(t *testing.T) {
	var hostCalls, procCalls int32
	mut := func(c *Config) {
		c.GPU = GPUAuto
		c.Host = hostStub(&hostCalls, host.Info{CPUPct: 42, HasCPU: true, Cores: 8}, nil)
		c.Proc = procStub(&procCalls, procstat.Info{PID: 7, RSSKB: 100, Threads: 3}, nil)
	}
	s := newC("http://192.0.2.10:8011", mut).Poll(context.Background(), t0)
	if atomic.LoadInt32(&hostCalls) != 0 || atomic.LoadInt32(&procCalls) != 0 || s.Host != nil || s.Proc != nil {
		t.Fatalf("remote URL read local sensors: host=%d proc=%d %+v %+v", hostCalls, procCalls, s.Host, s.Proc)
	}
	off := func(c *Config) { mut(c); c.GPU = GPUOff }
	newC("http://127.0.0.1:1", off).Poll(context.Background(), t0)
	if atomic.LoadInt32(&hostCalls) != 0 || atomic.LoadInt32(&procCalls) != 0 {
		t.Fatal("--no-gpu must not read local sensors")
	}
	on := func(c *Config) { mut(c); c.GPU = GPUOn }
	c := newC("http://192.0.2.10:8011", on) // --gpu forces local sources even for a remote URL
	c.Poll(context.Background(), t0)
	if atomic.LoadInt32(&hostCalls) != 1 || atomic.LoadInt32(&procCalls) != 1 {
		t.Fatalf("--gpu: host=%d proc=%d", hostCalls, procCalls)
	}
	c = newC("http://127.0.0.1:1", mut)
	var snap Snapshot
	for i := 0; i < 3; i++ {
		snap = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if snap.Host == nil || snap.Host.Cores != 8 || snap.Proc == nil || snap.Proc.PID != 7 ||
		!snap.HostAt.Equal(t0.Add(2*time.Second)) || !snap.ProcAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("loopback: %+v %+v", snap.Host, snap.Proc)
	}
	if len(snap.HostCPUHist) != 3 || snap.HostCPUHist[2] != 42 {
		t.Fatalf("HostCPUHist = %v", snap.HostCPUHist)
	}
}

func TestHostUnavailableLatchesOff(t *testing.T) {
	var hostCalls, procCalls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Host = hostStub(&hostCalls, host.Info{}, host.ErrUnavailable)
		c.Proc = procStub(&procCalls, procstat.Info{}, procstat.ErrUnavailable)
	})
	var s Snapshot
	for i := 0; i < 5; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if hostCalls != 1 || procCalls != 1 || s.Host != nil || s.Proc != nil {
		t.Fatalf("host=%d proc=%d %+v %+v", hostCalls, procCalls, s.Host, s.Proc)
	}
}

func TestProcNotFoundRetries(t *testing.T) {
	var procCalls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Proc = procStub(&procCalls, procstat.Info{}, procstat.ErrNotFound)
	})
	for i := 0; i <= 25; i++ { // 0s..25s
		s := c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
		if s.Proc != nil {
			t.Fatalf("Proc set while not found: %+v", s.Proc)
		}
	}
	if n := atomic.LoadInt32(&procCalls); n != 3 { // at 0s, 10s, 20s
		t.Fatalf("scans = %d, want 3", n)
	}
	// the process appears: it is picked up on the next retry and cleared again when it goes
	var found atomic.Bool
	c = newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Proc = func(time.Time) (procstat.Info, error) {
			if found.Load() {
				return procstat.Info{PID: 9}, nil
			}
			return procstat.Info{}, procstat.ErrNotFound
		}
	})
	c.Poll(context.Background(), t0)
	found.Store(true)
	if s := c.Poll(context.Background(), t0.Add(5*time.Second)); s.Proc != nil {
		t.Fatal("retried before 10s")
	}
	if s := c.Poll(context.Background(), t0.Add(10*time.Second)); s.Proc == nil || s.Proc.PID != 9 {
		t.Fatalf("not picked up at 10s: %+v", s.Proc)
	}
	found.Store(false)
	if s := c.Poll(context.Background(), t0.Add(11*time.Second)); s.Proc != nil {
		t.Fatalf("Proc kept after it vanished: %+v", s.Proc)
	}
}

// A host read that hangs must not block Poll, and is not started twice.
func TestLocalHungReadStartedOnce(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Host = func() (host.Info, error) { atomic.AddInt32(&calls, 1); <-release; return host.Info{}, nil }
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

func TestGPUHistories(t *testing.T) {
	gpus := []gpu.GPU{
		{UtilPct: 90, HasUtil: true, MemUsedMiB: 10 * 1024, MemTotalMiB: 20 * 1024, TempC: 60, HasTemp: true},
		{UtilPct: 50, HasUtil: true, MemUsedMiB: 20 * 1024, MemTotalMiB: 20 * 1024, TempC: 70, HasTemp: true},
	}
	c := newC("http://127.0.0.1:1", func(c *Config) { c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), gpus, nil) })
	pollGPU(t, c, t0)
	if s := pollGPU(t, c, t0.Add(time.Second)); len(s.GPUUtilHist) != 1 { // takes the first result
		t.Fatalf("one result taken, got %d samples", len(s.GPUUtilHist))
	}
	pollGPU(t, c, t0.Add(2*time.Second))      // starts the second query (one per 2s)
	s := pollGPU(t, c, t0.Add(3*time.Second)) // takes it
	if len(s.GPUUtilHist) != 2 {
		t.Fatalf("hist %v", s.GPUUtilHist)
	}
	last := func(h []float64) float64 { return h[len(h)-1] }
	if last(s.GPUUtilHist) != 70 || last(s.GPUMemHist) != 75 || last(s.GPUTempHist) != 70 {
		t.Fatalf("util %v mem %v temp %v", s.GPUUtilHist, s.GPUMemHist, s.GPUTempHist)
	}
}

// blockingQuery returns good results for the first n calls, then hangs until release is closed.
func blockingQuery(n int32, release chan struct{}) func(context.Context) ([]gpu.GPU, error) {
	var calls int32
	return func(context.Context) ([]gpu.GPU, error) {
		if atomic.AddInt32(&calls, 1) > n {
			<-release
			return nil, errors.New("late")
		}
		return pw(300), nil
	}
}

// A query that never returns counts as failing: GPU power leaves the totals 30s
// of polls after the first poll that saw it in flight.
func TestGPUPowerExpiresWhenQueryHangs(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, blockingQuery(1, release)
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
	})
	pollGPU(t, c, t0)                                        // starts the good query
	s := c.Poll(context.Background(), t0.Add(2*time.Second)) // takes it, starts the hung one
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("before: %+v", s)
	}
	time.Sleep(20 * time.Millisecond)
	s = c.Poll(context.Background(), t0.Add(4*time.Second)) // first poll to see the hung query: marker
	for i := 3; i <= 16; i++ {                              // the last poll at 32s: 28s after the marker, under 30s
		s = c.Poll(context.Background(), t0.Add(time.Duration(i*2)*time.Second))
	}
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("just under 30s: %+v", s)
	}
	s = c.Poll(context.Background(), t0.Add(34*time.Second)) // exactly 30s after the marker
	if s.HasGPUPower || s.GPUPowerW != 0 || s.PowerW != 100 || s.PowerHist[len(s.PowerHist)-1] != 100 {
		t.Fatalf("after 30s: %+v", s)
	}
}

// A pause while a query is in flight, which then returns normally, must not expire.
func TestGPUPowerSurvivesPauseWithQueryInFlight(t *testing.T) {
	gate := make(chan struct{})
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUOn
		c.GPUQuery = func(context.Context) ([]gpu.GPU, error) {
			if atomic.AddInt32(&calls, 1) == 2 {
				<-gate
			}
			return pw(300), nil
		}
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
	})
	pollGPU(t, c, t0)
	c.Poll(context.Background(), t0.Add(2*time.Second)) // takes #1, starts #2 which blocks
	time.Sleep(20 * time.Millisecond)
	s := c.Poll(context.Background(), t0.Add(62*time.Second)) // after the pause: sees #2 in flight (marker set)
	if !s.HasGPUPower {
		t.Fatalf("first post-pause poll: %+v", s)
	}
	close(gate)
	c.WaitGPU(time.Second)
	s = c.Poll(context.Background(), t0.Add(64*time.Second)) // #2 returns and is taken
	if !s.HasGPUPower || s.PowerW != 400 {
		t.Fatalf("after: %+v", s)
	}
}

// waitContainerIdle waits until no container lookup is in flight.
func waitContainerIdle(t *testing.T, c *Collector) {
	t.Helper()
	for i := 0; i < 400; i++ {
		c.loc.ct.mu.Lock()
		busy := c.loc.ct.busy
		c.loc.ct.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("container lookup never returned")
}

func ctStub(calls *int32, ports *[]int, info container.Info, err error) func(context.Context, int) (container.Info, error) {
	return func(_ context.Context, port int) (container.Info, error) {
		atomic.AddInt32(calls, 1)
		if ports != nil {
			*ports = append(*ports, port)
		}
		return info, err
	}
}

func TestContainerNeverBlocksPoll(t *testing.T) {
	release := make(chan struct{})
	var calls, running, maxRunning int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = func(context.Context, int) (container.Info, error) {
			atomic.AddInt32(&calls, 1)
			n := atomic.AddInt32(&running, 1)
			if n > atomic.LoadInt32(&maxRunning) {
				atomic.StoreInt32(&maxRunning, n)
			}
			<-release // stands in for a docker call that hangs
			atomic.AddInt32(&running, -1)
			return container.Info{Name: "x"}, nil
		}
	})
	for i := 0; i < 40; i++ { // 40s of polls
		start := time.Now()
		s := c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
		if d := time.Since(start); d > c.timeout+500*time.Millisecond {
			t.Fatalf("Poll %d took %v", i, d)
		}
		if s.Container != nil {
			t.Fatalf("Container set while the call is in flight: %+v", s.Container)
		}
	}
	close(release)
	if n := atomic.LoadInt32(&calls); n != 1 || atomic.LoadInt32(&maxRunning) != 1 {
		t.Fatalf("calls=%d maxRunning=%d, want one in-flight call", n, maxRunning)
	}
	waitContainerIdle(t, c)
	if s := c.Poll(context.Background(), t0.Add(41*time.Second)); s.Container == nil || s.Container.Name != "x" {
		t.Fatalf("result not picked up: %+v", s.Container)
	}
}

func TestContainerRefreshEvery10s(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = ctStub(&calls, nil, container.Info{Name: "x"}, nil)
	})
	var s Snapshot
	for i := 0; i < 30; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
		waitContainerIdle(t, c)
	}
	if n := atomic.LoadInt32(&calls); n < 3 || n > 4 {
		t.Fatalf("calls = %d, want 3..4", n)
	}
	if s.Container == nil || !s.ContainerAt.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("%+v at %v", s.Container, s.ContainerAt)
	}
}

func TestContainerUnavailableLatches(t *testing.T) {
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = ctStub(&calls, nil, container.Info{}, container.ErrUnavailable)
	})
	var s Snapshot
	for i := 0; i < 60; i += 5 {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
		waitContainerIdle(t, c)
	}
	if atomic.LoadInt32(&calls) != 1 || s.Container != nil {
		t.Fatalf("calls=%d %+v", calls, s.Container)
	}
}

func TestContainerNotFoundRetries(t *testing.T) {
	var found atomic.Bool
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = func(context.Context, int) (container.Info, error) {
			atomic.AddInt32(&calls, 1)
			if found.Load() {
				return container.Info{Name: "llama"}, nil
			}
			return container.Info{}, container.ErrNotFound
		}
	})
	poll := func(sec int) Snapshot {
		s := c.Poll(context.Background(), t0.Add(time.Duration(sec)*time.Second))
		waitContainerIdle(t, c)
		return s
	}
	poll(0)
	if s := poll(10); s.Container != nil {
		t.Fatalf("set while not found: %+v", s.Container)
	}
	found.Store(true)
	poll(20) // starts the successful call
	if s := poll(21); s.Container == nil || s.Container.Name != "llama" {
		t.Fatalf("not picked up: %+v (calls %d)", s.Container, calls)
	}
	found.Store(false)
	poll(30)
	if s := poll(31); s.Container != nil {
		t.Fatalf("kept after the container went away: %+v", s.Container)
	}
}

func TestContainerLoopbackOnly(t *testing.T) {
	var calls int32
	var ports []int
	mut := func(c *Config) {
		c.GPU = GPUAuto
		c.Container = ctStub(&calls, &ports, container.Info{Name: "x"}, nil)
	}
	s := newC("http://192.0.2.10:8011", mut).Poll(context.Background(), t0)
	if atomic.LoadInt32(&calls) != 0 || s.Container != nil {
		t.Fatal("remote URL read the container")
	}
	newC("http://127.0.0.1:1", func(c *Config) { mut(c); c.GPU = GPUOff }).Poll(context.Background(), t0)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("--no-gpu must not read the container")
	}
	c := newC("http://192.0.2.10:8011", func(c *Config) { mut(c); c.GPU = GPUOn })
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	if atomic.LoadInt32(&calls) != 1 || ports[0] != 8011 {
		t.Fatalf("--gpu: calls=%d ports=%v", calls, ports)
	}
}

func TestContainerHostPort(t *testing.T) {
	for url, want := range map[string]int{
		"http://localhost:8011": 8011, "http://localhost": 80, "https://localhost": 443,
		"http://[::1]:9": 9, "http://localhost:x": 0, "::bad": 0,
	} {
		if got := hostPort(url); got != want {
			t.Errorf("hostPort(%q) = %d, want %d", url, got, want)
		}
	}
}

// A lookup that never returns clears the value once polls have seen it in flight
// for 30s, and ContainerAt stays at the last good result.
func TestContainerHangExpiresValue(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = func(context.Context, int) (container.Info, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return container.Info{Name: "x"}, nil
			}
			<-release
			return container.Info{}, nil
		}
	})
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	if s := c.Poll(context.Background(), t0.Add(time.Second)); s.Container == nil {
		t.Fatal("first result not shown")
	}
	c.Poll(context.Background(), t0.Add(10*time.Second)) // second call starts and hangs
	if s := c.Poll(context.Background(), t0.Add(11*time.Second)); s.Container == nil || !s.ContainerAt.Equal(t0) {
		t.Fatalf("expired too early: %+v at %v", s.Container, s.ContainerAt)
	}
	if s := c.Poll(context.Background(), t0.Add(40*time.Second)); s.Container == nil {
		t.Fatal("expired before 30s of being seen busy")
	}
	if s := c.Poll(context.Background(), t0.Add(41*time.Second)); s.Container != nil {
		t.Fatalf("stale value still shown: %+v", s.Container)
	}
}

func TestContainerOtherErrorKeepsValue(t *testing.T) {
	var fail atomic.Bool
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = func(context.Context, int) (container.Info, error) {
			if fail.Load() {
				return container.Info{}, errors.New("boom")
			}
			return container.Info{Name: "x"}, nil
		}
	})
	c.Poll(context.Background(), t0)
	waitContainerIdle(t, c)
	fail.Store(true)
	c.Poll(context.Background(), t0.Add(10*time.Second))
	waitContainerIdle(t, c)
	s := c.Poll(context.Background(), t0.Add(11*time.Second))
	if s.Container == nil || !s.ContainerAt.Equal(t0) {
		t.Fatalf("%+v at %v", s.Container, s.ContainerAt)
	}
}

// A daemon that is down clears the value but never latches the source off.
func TestContainerDaemonDownClearsAndRetries(t *testing.T) {
	var down atomic.Bool
	var calls int32
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU = GPUAuto
		c.Container = func(context.Context, int) (container.Info, error) {
			atomic.AddInt32(&calls, 1)
			if down.Load() {
				return container.Info{}, container.ErrDaemonDown
			}
			return container.Info{Name: "llama"}, nil
		}
	})
	poll := func(sec int) Snapshot {
		s := c.Poll(context.Background(), t0.Add(time.Duration(sec)*time.Second))
		waitContainerIdle(t, c)
		return s
	}
	poll(0)
	if s := poll(1); s.Container == nil {
		t.Fatal("first result not shown")
	}
	down.Store(true)
	poll(10)
	if s := poll(11); s.Container != nil {
		t.Fatalf("value kept while the daemon is down: %+v", s.Container)
	}
	down.Store(false)
	poll(20)
	if s := poll(21); s.Container == nil || s.Container.Name != "llama" {
		t.Fatalf("not picked up after the daemon came back: %+v (calls %d)", s.Container, calls)
	}
}

// Each history ring reports when the period its oldest sample covers began (the
// push before it; the first sample's own time), so the UI can state the real span
// after an interval change.
func TestHistSpans(t *testing.T) {
	gpus := []gpu.GPU{{UtilPct: 90, HasUtil: true, MemUsedMiB: 1024, MemTotalMiB: 2048, TempC: 60, HasTemp: true}}
	c := newC("http://127.0.0.1:1", func(c *Config) {
		c.GPU, c.GPUQuery = GPUOn, gpuStub(new(int32), gpus, nil)
		c.CPUPower = cpuStub(nil, 100, 100, true, nil)
		c.Host = hostStub(nil, host.Info{CPUPct: 5, HasCPU: true}, nil)
	})
	var s Snapshot
	at := t0
	for i := 0; i < 5; i++ { // 1 s polls, then 10 s polls
		s = pollGPU(t, c, at)
		at = at.Add(time.Second)
	}
	for i := 0; i < 2; i++ {
		at = at.Add(9 * time.Second)
		s = pollGPU(t, c, at)
		at = at.Add(time.Second)
	}
	if !s.GenHistFrom.Equal(t0) || s.At.Sub(s.GenHistFrom) != 24*time.Second {
		t.Errorf("gen from %v, span %v", s.GenHistFrom, s.At.Sub(s.GenHistFrom))
	}
	if !s.HostCPUHistFrom.Equal(t0) || !s.PowerHistFrom.Equal(t0) || !s.GPUHistFrom.Equal(t0) {
		t.Errorf("host from %v power from %v gpu from %v", s.HostCPUHistFrom, s.PowerHistFrom, s.GPUHistFrom)
	}
	// once the ring is full the oldest sample's period starts at the push before it
	c = newC("http://127.0.0.1:1", nil)
	for i := 0; i < 305; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	if len(s.GenHist) != 300 || s.At.Sub(s.GenHistFrom) != 300*time.Second {
		t.Errorf("full ring: %d samples, span %v", len(s.GenHist), s.At.Sub(s.GenHistFrom))
	}
	// one start per sample, oldest first: the newest 10 samples began 10 s ago
	if st := s.GenHistStarts; len(st) != 300 || !st[0].Equal(s.GenHistFrom) || s.At.Sub(st[290]) != 10*time.Second {
		t.Errorf("starts: %d, first %v, span of the newest 10 %v", len(st), st[0], s.At.Sub(st[290]))
	}
}

func TestSpanRingValues(t *testing.T) {
	r := newSpanRing(3)
	if r.values() != nil {
		t.Fatal("empty ring")
	}
	for i := range 5 {
		r.push(t0.Add(time.Duration(i) * time.Second))
	}
	v := r.values() // pushes at 2, 3, 4: their periods began at 1, 2, 3
	if len(v) != 3 || !v[0].Equal(t0.Add(time.Second)) || !v[2].Equal(t0.Add(3*time.Second)) {
		t.Fatalf("%v", v)
	}
}
