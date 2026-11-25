// Package collect polls a llama-server (and optionally local GPUs) and folds the
// results into one Snapshot per poll.
package collect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"llamadash/internal/container"
	"llamadash/internal/cpupower"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/hwmon"
	"llamadash/internal/llama"
	"llamadash/internal/procstat"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

// GPUMode says when the GPU is queried.
type GPUMode int

const (
	GPUAuto GPUMode = iota // only when the server URL is loopback
	GPUOn
	GPUOff
)

const (
	histLen   = 300
	recentMax = 50  // finished requests kept
	minRate   = 0.5 // tok/s below which J/token is not meaningful
)

// Config configures a Collector.
type Config struct {
	URL, APIKey string
	Interval    time.Duration
	GPU         GPUMode
	GPUQuery    func(context.Context) ([]gpu.GPU, error)                            // nil -> gpu.Query; tests inject
	CPUPower    func(now time.Time) (watts, joules float64, ok bool, err error)     // nil -> cpupower.Reader on the default root
	Host        func() (host.Info, error)                                           // nil -> host.Reader on /proc
	Proc        func(now time.Time) (procstat.Info, error)                          // nil -> procstat.Reader for llama-server
	Hwmon       func() (hwmon.Reading, error)                                       // nil -> hwmon.Reader on /sys/class/hwmon
	Container   func(ctx context.Context, hostPort int) (container.Info, error)     // used when Docker is nil; no list
	Docker      func(ctx context.Context, hostPort int) (container.Overview, error) // nil -> container.Read (or Container)

	KWhPrice   float64  // 0 = cost off
	Currency   string   // "" or a three-letter code
	Store      Recorder // nil = no database
	DBOpenErr  error    // the database failed to open: DBStatus says why
	DBReadOnly bool     // another llamadash records to the database: Store only reads
	OneShot    bool     // --once / --frame: two polls, no integrated interval to speak of
	PSUWatts   float64  // the power supply's maximum wattage; 0 = not set

	// KnownFans are the identities (hwmon.Fan.Key) of fans seen spinning in
	// earlier runs: shown from the first reading, stopped or missing included.
	KnownFans []string
	// OnNewFans, when set, gets the whole known list (at most 64) each time a fan
	// spins for the first time. It is called from Poll and must not block.
	OnNewFans func(known []string)
}

// Snapshot is the state after one poll.
type Snapshot struct {
	At               time.Time
	URL              string
	Reachable        bool
	UnreachableSince time.Time // zero when reachable
	Loading          bool      // server answered 503
	Health           llama.Health
	Props            *llama.Props // nil until first success
	Slots            []stats.SlotView
	SlotsAt          time.Time // last successful /slots
	SlotsHint        string    // non-empty when /slots is unusable
	Metrics          llama.Metrics
	MetricsHint      string
	GenRate          float64 // sum over slots
	PromptRate       float64
	GenHist          []float64 // one sample per poll, max 300
	PromptHist       []float64
	GPUs             []gpu.GPU // nil -> panel hidden
	GPUAt            time.Time
	Host             *host.Info // nil -> panel hidden
	HostAt           time.Time
	Proc             *procstat.Info // nil: llama-server not found on this host
	ProcAt           time.Time
	Container        *container.Info   // nil: no container found, or not read
	ContainerAt      time.Time         // last successful lookup
	Containers       []container.Entry // every container on the host; nil: none, or not read
	ContainersAt     time.Time         // last successful list
	Fans             []Fan             // motherboard fans that have spun at least once; nil: none
	CPUTempC         float64           // CPU package temperature, valid when HasCPUTemp
	HasCPUTemp       bool              // a CPU temperature sensor was read
	HwmonAt          time.Time         // last successful hardware-monitor read (Fans, CPUTempC)
	HostCPUHist      []float64         // max 300; one sample per host read with a CPU figure
	GPUUtilHist      []float64         // max 300; one sample per GPU result taken: mean util
	GPUMemHist       []float64         // total VRAM used / total, percent
	GPUTempHist      []float64         // hottest GPU
	Recent           []stats.Finished  // finished requests, newest first, max 50

	// When the period of each ring's oldest sample began (zero: empty ring); the
	// span a ring covers is At minus this, whatever the poll interval was.
	GenHistFrom     time.Time // GenHist and PromptHist
	PowerHistFrom   time.Time
	GPUHistFrom     time.Time // GPUUtilHist, GPUMemHist, GPUTempHist
	HostCPUHistFrom time.Time

	// When each sample's period began, oldest first, one per sample of the rings
	// above (a chart showing only the newest samples states their span from these).
	GenHistStarts, PowerHistStarts, GPUHistStarts, HostCPUHistStarts []time.Time

	HasPower       bool      // any power source present
	PowerW         float64   // total: GPU + CPU, latest sample
	PowerHist      []float64 // total power, one sample per fresh reading, max 300
	EnergyWh       float64   // total energy since the dashboard started
	JoulesPerToken float64   // PowerW / GenRate; 0 when GenRate < 0.5
	HasCPUPower    bool
	CPUPowerW      float64
	HasGPUPower    bool    // GPU power is part of the totals
	GPUPowerW      float64 // sum over GPUs reporting power
	CPUEnergyWh    float64
	GPUEnergyWh    float64
	PSUWatts       float64 // the power supply's maximum wattage (a setting); 0 = not set

	Cost                  Cost
	Today, Month, AllTime store.Totals // HasData false without a store
	DBStatus              string       // "" when healthy or disabled; otherwise "off (<short reason>)"
	DBReadOnly            bool         // another llamadash records to the database; this one only reads it
	OneShot               bool         // --once / --frame: "since start" figures are meaningless
	GenTokensSinceStart   int64
}

// Collector keeps the state between polls.
type Collector struct {
	cfg      Config
	timeout  time.Duration
	client   *llama.Client
	tracker  *stats.Tracker
	gpuQuery func(context.Context) ([]gpu.GPU, error)
	cpuRead  func(time.Time) (float64, float64, bool, error)
	gpuHost  bool // URL host is loopback

	mu sync.Mutex // Poll never overlaps itself

	props      *llama.Props
	propsDirty bool // refetch /props (first poll, or after an outage)
	reachable  bool
	downSince  time.Time
	slots      []stats.SlotView
	slotsAt    time.Time
	recent     []stats.Finished // newest first, max recentMax
	genHist    *stats.Ring
	genSpan    *spanRing // GenHist and PromptHist
	powerSpan  *spanRing
	promptHist *stats.Ring

	gpuDead    bool
	lastQuery  time.Time
	gpus       []gpu.GPU
	gpuAt      time.Time
	busySeen   time.Time // first poll that saw the current in-flight query busy; zero when none
	gpuTakenAt time.Time // query time of the last result taken, good or not

	gpuMu     sync.Mutex // guards the fields below, written by the query goroutine
	gpuBusy   bool       // a query is running (cleared only when it really returns)
	gpuDone   chan struct{}
	gpuLatest *gpuResult // delivered, not yet taken by Poll

	cpuBusy atomic.Bool // a CPU power read is running
	loc     localSensors

	powerHist *stats.Ring
	havePower bool
	powerW    float64
	prevAt    time.Time // previous successful power sample
	prevW     float64
	havePrev  bool
	energyWh  float64 // GPU energy

	cpuDead     bool
	haveCPU     bool
	cpuW        float64
	cpuEnergyWh float64
	gpuSecs     float64   // time covered by the GPU energy integration
	cpuSecs     float64   // time covered by the CPU energy readings
	cpuPrevAt   time.Time // last CPU read without error

	hist history // windows sent to the store (history.go)
}

// timeoutFor is max(min(interval, 2s) * 0.8, 1s): short intervals still give a
// busy server a full second to answer.
func timeoutFor(interval time.Duration) time.Duration {
	return max(min(interval, 2*time.Second)*8/10, time.Second)
}

// New creates a collector.
func New(cfg Config) *Collector {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	c := &Collector{
		cfg:        cfg,
		timeout:    timeoutFor(cfg.Interval),
		tracker:    stats.NewTracker(),
		gpuQuery:   cfg.GPUQuery,
		propsDirty: true,
		genHist:    stats.NewRing(histLen),
		promptHist: stats.NewRing(histLen),
		powerHist:  stats.NewRing(histLen),
		genSpan:    newSpanRing(histLen),
		powerSpan:  newSpanRing(histLen),
		gpuHost:    isLoopback(cfg.URL),
	}
	if c.gpuQuery == nil {
		c.gpuQuery = gpu.Query
	}
	c.cpuRead = cfg.CPUPower
	if c.cpuRead == nil {
		c.cpuRead = cpupower.NewReader("").Read
	}
	c.initLocal()
	c.client = llama.New(cfg.URL, cfg.APIKey, c.timeout)
	return c
}

// SetInterval changes the poll interval (clamped to 250ms..10s) and with it the
// per-request timeout. It waits for a Poll in progress, so it is safe between
// polls. An unchanged interval is a no-op (the HTTP client is kept).
func (c *Collector) SetInterval(d time.Duration) {
	d = min(max(d, 250*time.Millisecond), 10*time.Second)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d == c.cfg.Interval {
		return
	}
	c.cfg.Interval = d
	c.timeout = timeoutFor(d)
	c.client = llama.New(c.cfg.URL, c.cfg.APIKey, c.timeout)
}

func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isTransport reports whether err means no HTTP response was received.
func isTransport(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}

type result[T any] struct {
	v   T
	err error
}

// Poll queries the server and returns the new snapshot. A GPU query runs in the
// background: its result appears in the first Poll after it returns (see WaitGPU).
func (c *Collector) Poll(ctx context.Context, now time.Time) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	needProps := c.props == nil || c.propsDirty
	needMetrics := c.props == nil || c.props.EndpointMetrics
	powerFresh := false
	if r := c.takeGPU(); r != nil && c.applyGPU(*r) {
		powerFresh = true
	}
	c.expireGPUPower(now)
	if c.gpuDue(now) {
		c.startGPU(now)
	}
	wantCPU := c.cpuDue()
	wantLocal := c.localDue(now)
	c.pollContainer(now)

	var (
		wg      sync.WaitGroup
		health  result[llama.Health]
		props   result[llama.Props]
		slots   result[[]llama.Slot]
		metrics result[llama.Metrics]
		cres    cpuResult
		lres    localResult
	)
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	run(func() { health.v, health.err = c.client.Health(ctx) })
	run(func() { slots.v, slots.err = c.client.Slots(ctx) })
	if needProps {
		run(func() { props.v, props.err = c.client.Props(ctx) })
	}
	if needMetrics {
		run(func() { metrics.v, metrics.err = c.client.Metrics(ctx) })
	}
	if wantCPU {
		run(func() { cres = c.runCPU(ctx, now) })
	}
	if wantLocal {
		run(func() { lres = c.runLocal(ctx, now) })
	}
	wg.Wait()

	snap := Snapshot{At: now, URL: c.cfg.URL}
	reachable := health.err == nil || !isTransport(health.err)
	snap.Reachable = reachable
	if reachable {
		c.onReachable(&snap, health, props, needProps, slots, metrics, needMetrics, now)
	} else {
		c.onUnreachable(now)
		snap.UnreachableSince = c.downSince
		snap.MetricsHint, snap.SlotsHint = "", ""
	}
	c.reachable = reachable
	fresh := reachable && slots.err == nil

	genRate, promptRate := 0.0, 0.0
	if fresh {
		for _, s := range c.slots {
			genRate += s.GenRate
			promptRate += s.PromptRate
		}
	}
	c.genHist.Push(genRate)
	c.genSpan.push(now)
	c.promptHist.Push(promptRate)
	snap.GenRate, snap.PromptRate = genRate, promptRate
	snap.GenHist, snap.PromptHist = c.genHist.Values(), c.promptHist.Values()
	snap.GenHistFrom, snap.GenHistStarts = c.genSpan.from(), c.genSpan.values()

	if props.err == nil && needProps && reachable {
		p := props.v
		c.props, c.propsDirty = &p, false
	}
	if c.props != nil {
		p := *c.props
		snap.Props = &p
		if !p.EndpointMetrics && reachable {
			snap.Metrics = nil
			snap.MetricsHint = metricsHint
		}
	}
	snap.Slots = append([]stats.SlotView(nil), c.slots...)
	if !fresh { // retained views are stale: never present their rates as live
		for i := range snap.Slots {
			snap.Slots[i].GenRate, snap.Slots[i].PromptRate = 0, 0
		}
	}
	snap.SlotsAt = c.slotsAt
	snap.Recent = append([]stats.Finished(nil), c.recent...)

	if wantCPU && c.applyCPU(cres, now) {
		powerFresh = true
	}
	if powerFresh {
		c.recordPower(now)
	}
	if wantLocal {
		c.applyLocal(lres, now)
	}
	snap.GPUs, snap.GPUAt = c.gpus, c.gpuAt
	c.fillLocal(&snap)
	c.fillPower(&snap, genRate)
	c.recordHistory(&snap, now)
	return snap
}

const (
	metricsHint = "start llama-server with --metrics for queue depth and totals"
	slotsOffMsg = "slots endpoint is disabled: start llama-server with --slots"
	slotsAuth   = "server requires an API key: use --api-key or LLAMA_API_KEY"
	slotsHTTP   = "HTTP error from /slots"
)

func (c *Collector) onUnreachable(now time.Time) {
	if c.downSince.IsZero() {
		c.downSince = now
	}
	c.tracker.Reset()
	c.propsDirty = true // refetch /props once the server is back
}

func (c *Collector) onReachable(snap *Snapshot, health result[llama.Health], props result[llama.Props],
	needProps bool, slots result[[]llama.Slot], metrics result[llama.Metrics], needMetrics bool, now time.Time) {
	c.downSince = time.Time{}
	snap.Health = health.v
	switch {
	case errors.Is(health.err, llama.ErrLoading):
		snap.Loading = true
		if snap.Health.Status == "" {
			snap.Health.Status = "loading model"
		}
	case health.err != nil: // an error status or a body that is not /health JSON
		snap.Health.Status = "error"
	}

	// slots
	switch {
	case slots.err == nil:
		views, _, _ := c.tracker.Update(now, slots.v)
		c.slots, c.slotsAt = views, now
		c.takeTracker()
	case errors.Is(slots.err, llama.ErrLoading):
		// keep previous data; the server is still starting
	case errors.Is(slots.err, llama.ErrDisabled):
		c.dropSlots()
		snap.SlotsHint = slotsOffMsg
	case errors.Is(slots.err, llama.ErrUnauthorized):
		c.dropSlots()
		snap.SlotsHint = slotsAuth
	case isTransport(slots.err):
		// answered /health but not /slots in time: keep stale data
	case errors.Is(slots.err, llama.ErrHTTPStatus):
		c.dropSlots()
		snap.SlotsHint = slotsHTTP
	default: // the response did not parse
		c.dropSlots()
		build := ""
		switch {
		case props.err == nil && needProps:
			build = props.v.BuildInfo
		case c.props != nil:
			build = c.props.BuildInfo
		}
		if build == "" {
			build = "unknown"
		}
		snap.SlotsHint = fmt.Sprintf("unrecognised /slots response (llama.cpp build %s)", build)
	}

	// metrics
	if needMetrics {
		switch {
		case metrics.err == nil:
			snap.Metrics = metrics.v
		case errors.Is(metrics.err, llama.ErrDisabled):
			snap.MetricsHint = metricsHint
		}
	}
}

func (c *Collector) dropSlots() {
	c.slots, c.slotsAt = nil, time.Time{}
	c.tracker.Reset()
}

// addRecent prepends finished (oldest first) to the newest-first ring.
func (c *Collector) addRecent(finished []stats.Finished) {
	if len(finished) == 0 {
		return
	}
	n := min(len(finished), recentMax)
	merged := make([]stats.Finished, 0, recentMax)
	for i := len(finished) - 1; i >= len(finished)-n; i-- {
		merged = append(merged, finished[i])
	}
	merged = append(merged, c.recent...)
	c.recent = merged[:min(len(merged), recentMax)]
}
