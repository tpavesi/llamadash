package collect

import (
	"errors"
	"math"
	"sync"
	"time"

	"llamadash/internal/stats"
	"llamadash/internal/store"
)

// Recorder is the history database as the collector uses it; *store.Store implements
// it. Add* never block; the other methods may wait on the database and are never
// called by Poll itself (only by Start and by background reads).
type Recorder interface {
	SetTariff(time.Time, float64, string) error
	AddSample(store.Sample)
	AddRequest(stats.Finished)
	AddUsage(time.Time, store.Usage)
	Err() error
	RecentRequests(int) ([]stats.Finished, error)
	Totals(time.Time) (today, month, all store.Totals, err error)
}

// Cost is the energy cost at the configured price.
type Cost struct {
	Enabled    bool
	Currency   string
	Price      float64 // per kWh
	SinceStart float64 // energy since start × price
	PerDay     float64 // mean power since start × 24h × price
	PerMTok    float64 // SinceStart per million tokens generated since start; 0 under 1000 tokens
}

const (
	windowEvery   = 10 * time.Second // one sample and one usage delta per window
	suspendGap    = 60 * time.Second // a longer gap between polls closes the window at its last poll
	totalsEvery   = 10 * time.Second // between background Totals reads
	minPerMTokens = 1000             // generated tokens below which PerMTok is 0
	maxWindowReqs = 256              // finished requests held per window
)

// errStoreTimeout means a start-up read did not finish within its bound.
var errStoreTimeout = errors.New("collect: the database did not answer in time")

// history accumulates what is sent to the store: one window of polls at a time.
type history struct {
	poll     store.Usage // this poll's tokens and requests, folded in by recordHistory
	finished []stats.Finished

	open                 bool
	start, last          time.Time // first and latest poll of the window
	n                    int
	gen, prompt          float64 // sums over the window's polls
	gpuW, cpuW           float64
	hostCPU, procCPU     float64
	nGPUW, nCPUW         int
	nHostCPU, nProcCPU   int
	latest               store.Sample // the window's latest values, for the fields that are not averaged
	usage                store.Usage
	requests             []stats.Finished
	markGPUWh, markCPUWh float64 // cumulative energy at the previous poll
	genTotal             int64   // tokens generated since start

	// background reads
	mu        sync.Mutex // guards busy, result and pruneErr, written by other goroutines
	busy      bool
	result    *storeRead
	pruneErr  error // the last prune failed
	lastRead  time.Time
	readErr   error
	started   time.Time // Start's now; seeded requests from this run are dropped
	today     store.Totals
	month     store.Totals
	all       store.Totals
	haveTotal bool
}

// storeRead is the result of a background read.
type storeRead struct {
	seeded            bool
	recent            []stats.Finished
	totals            bool
	today, month, all store.Totals
	err               error
}

// takeTracker takes the finished requests and token deltas of this poll. It runs
// right after the tracker update, under the collector's lock.
func (c *Collector) takeTracker() {
	fin := c.tracker.Drain()
	c.addRecent(fin)
	// Tokens a request produces after its last busy sample are never seen (polling
	// cannot see them), so daily token totals are a slight undercount; the per-request
	// Generated figures are exact to the last busy sample.
	gen, prompt := c.tracker.TakeTokenDeltas()
	h := &c.hist
	h.poll.GenTokens += int64(gen)
	h.poll.PromptTokens += int64(prompt)
	h.poll.Requests += int64(len(fin))
	if c.cfg.Store != nil {
		h.finished = append(h.finished, fin...)
	}
}

// Start loads the recent requests and the totals from the store and records the
// tariff when a price is set, waiting at most wait. Call it once, before the first
// Poll. It reports whether the reads finished in time; when they did not, they carry
// on in the background (the next Poll picks them up) and DBStatus says so meanwhile.
func (c *Collector) Start(now time.Time, wait time.Duration) bool {
	rec := c.cfg.Store
	if rec == nil {
		return true
	}
	c.mu.Lock()
	h := &c.hist
	h.started, h.lastRead = now, now
	h.mu.Lock()
	h.busy = true
	h.mu.Unlock()
	c.mu.Unlock()

	price, currency := c.cfg.KWhPrice, c.cfg.Currency
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := storeRead{seeded: true, totals: true}
		var errs [3]error
		r.recent, errs[0] = rec.RecentRequests(recentMax)
		r.today, r.month, r.all, errs[1] = rec.Totals(now)
		r.totals = errs[1] == nil
		if price > 0 {
			errs[2] = rec.SetTariff(now, price, currency)
		}
		r.err = errors.Join(errs[:]...)
		c.deliver(&r)
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		c.mu.Lock()
		h.readErr = errStoreTimeout
		c.mu.Unlock()
		return false
	}
}

// SetPruneErr records the result of the latest prune: while it is an error,
// DBStatus says so (unless a write or read error is already shown). Safe to call
// from any goroutine.
func (c *Collector) SetPruneErr(err error) {
	h := &c.hist
	h.mu.Lock()
	h.pruneErr = err
	h.mu.Unlock()
}

// FlushHistory sends the open window to the store as a window flush at now would
// (sample, finished requests, usage dated now; after a long gap, dated its last
// poll). Call it on exit, before the store is closed. Without a store or an open
// window it sends nothing, so a second call is a no-op.
func (c *Collector) FlushHistory(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := &c.hist
	if c.cfg.Store == nil || !h.open {
		return
	}
	end := now
	if d := now.Sub(h.last); d > suspendGap || d < 0 {
		end = h.last
	}
	c.flushWindow(end)
}

// StartRecording turns a read-only collector into the recorder, once this process
// holds the database's lock. The open window holds polls from the read-only
// period (at most one window, 10 s): it is dropped, not written. Then enable makes
// the store write, and DBReadOnly clears. Safe to call from any goroutine.
func (c *Collector) StartRecording(enable func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetWindow()
	enable()
	c.cfg.DBReadOnly = false
}

// deliver hands a background read to the next Poll.
func (c *Collector) deliver(r *storeRead) {
	h := &c.hist
	h.mu.Lock()
	h.result, h.busy = r, false
	h.mu.Unlock()
}

// pollStore folds in a background read delivered since the last poll and starts a
// Totals read when one is due. It never waits for the read. It reports whether
// stored requests were added to recent.
func (c *Collector) pollStore(now time.Time) (seeded bool) {
	h := &c.hist
	h.mu.Lock()
	r, busy := h.result, h.busy
	h.result = nil
	h.mu.Unlock()
	if r != nil {
		h.readErr = r.err
		if r.seeded {
			c.seedRecent(r.recent)
			seeded = true
		}
		if r.totals {
			h.today, h.month, h.all, h.haveTotal = r.today, r.month, r.all, true
		}
	}
	if busy || (!h.lastRead.IsZero() && now.Sub(h.lastRead) < totalsEvery && !now.Before(h.lastRead)) {
		return seeded
	}
	h.lastRead = now
	h.mu.Lock()
	h.busy = true
	h.mu.Unlock()
	rec := c.cfg.Store
	go func() {
		r := storeRead{totals: true}
		r.today, r.month, r.all, r.err = rec.Totals(now)
		r.totals = r.err == nil
		c.deliver(&r)
	}()
	return seeded
}

// seedRecent appends stored requests (newest first) behind the ones seen in this run.
func (c *Collector) seedRecent(stored []stats.Finished) {
	for _, f := range stored {
		if len(c.recent) >= recentMax {
			return
		}
		if !c.hist.started.IsZero() && !f.At.Before(c.hist.started) {
			continue // recorded by this run after Start: already in recent
		}
		c.recent = append(c.recent, f)
	}
}

// recordHistory folds this poll into the window, sends a window to the store when it
// is complete, and fills the cost, totals and database fields of snap.
func (c *Collector) recordHistory(snap *Snapshot, now time.Time) {
	h := &c.hist
	// Energy of this poll: the difference of the on-screen integration, so the stored
	// totals and the "since start" figure cannot drift apart.
	dGPU, dCPU := c.energyWh-h.markGPUWh, c.cpuEnergyWh-h.markCPUWh
	h.markGPUWh, h.markCPUWh = c.energyWh, c.cpuEnergyWh
	h.genTotal += h.poll.GenTokens
	poll, fin := h.poll, h.finished
	h.poll, h.finished = store.Usage{}, nil

	if c.cfg.Store != nil {
		if h.open {
			switch d := now.Sub(h.last); {
			case d > suspendGap || d < 0: // suspend or clock jump: nothing is sent for the gap
				c.flushWindow(h.last)
			case now.Sub(h.start) >= windowEvery:
				c.flushWindow(now)
			}
		}
		poll.GPUEnergyWh, poll.CPUEnergyWh = dGPU, dCPU
		c.addToWindow(snap, now, poll, fin)
		if c.pollStore(now) {
			snap.Recent = append([]stats.Finished(nil), c.recent...)
		}
	}
	c.fillHistory(snap)
}

// addToWindow folds one poll into the window, opening one if needed.
func (c *Collector) addToWindow(snap *Snapshot, now time.Time, poll store.Usage, fin []stats.Finished) {
	h := &c.hist
	if !h.open {
		h.open, h.start = true, now
	}
	h.last = now
	h.n++
	h.gen += snap.GenRate
	h.prompt += snap.PromptRate
	if snap.HasGPUPower {
		h.gpuW += snap.GPUPowerW
		h.nGPUW++
	}
	if snap.HasCPUPower {
		h.cpuW += snap.CPUPowerW
		h.nCPUW++
	}
	if snap.Host != nil && snap.Host.HasCPU {
		h.hostCPU += snap.Host.CPUPct
		h.nHostCPU++
	}
	if snap.Proc != nil && snap.Proc.HasCPU {
		h.procCPU += snap.Proc.CPUPct
		h.nProcCPU++
	}
	h.latest = latestValues(snap)
	h.usage = addUsage(h.usage, poll)
	for _, f := range fin {
		if len(h.requests) >= maxWindowReqs {
			h.requests = append(h.requests[:0], h.requests[1:]...)
		}
		h.requests = append(h.requests, f)
	}
}

// flushWindow sends the window to the store: one sample stamped with the window's
// start, its finished requests, and its usage dated end.
func (c *Collector) flushWindow(end time.Time) {
	h := &c.hist
	rec := c.cfg.Store
	x := h.latest
	x.At = h.start
	mean := func(sum float64, n int) float64 {
		if n == 0 {
			return math.NaN() // no reading in the window: stored as no data, not 0
		}
		return sum / float64(n)
	}
	x.GenRate, x.PromptRate = mean(h.gen, h.n), mean(h.prompt, h.n)
	x.GPUPowerW, x.CPUPowerW = mean(h.gpuW, h.nGPUW), mean(h.cpuW, h.nCPUW)
	x.HostCPUPct, x.ProcCPUPct = mean(h.hostCPU, h.nHostCPU), mean(h.procCPU, h.nProcCPU)
	rec.AddSample(x)
	for _, f := range h.requests {
		rec.AddRequest(f)
	}
	u := h.usage
	if p := c.cfg.KWhPrice; p > 0 {
		u.Cost = (u.GPUEnergyWh + u.CPUEnergyWh) / 1000 * p // one price per run
	}
	rec.AddUsage(end, u)
	c.resetWindow()
}

// resetWindow empties the window.
func (c *Collector) resetWindow() {
	h := &c.hist
	h.open, h.n = false, 0
	h.gen, h.prompt, h.gpuW, h.cpuW, h.hostCPU, h.procCPU = 0, 0, 0, 0, 0, 0
	h.nGPUW, h.nCPUW, h.nHostCPU, h.nProcCPU = 0, 0, 0, 0
	h.usage, h.requests = store.Usage{}, nil
}

// latestValues are the sample fields taken from the window's latest poll; a field
// whose source is absent is NaN (the memory figures: their Has flag false).
func latestValues(snap *Snapshot) store.Sample {
	nan := math.NaN()
	x := store.Sample{GPUUtil: nan, GPUMemPct: nan, GPUTempC: nan}
	if v, ok := snap.Metrics["requests_processing"]; ok {
		x.Processing = int(v)
	} else {
		for _, s := range snap.Slots {
			if s.State != stats.Idle {
				x.Processing++
			}
		}
	}
	if v, ok := snap.Metrics["requests_deferred"]; ok {
		x.Queued = int(v)
	}
	var util, used, total float64
	var nUtil int
	for _, g := range snap.GPUs {
		if g.HasUtil {
			util += g.UtilPct
			nUtil++
		}
		if g.HasTemp && (math.IsNaN(x.GPUTempC) || g.TempC > x.GPUTempC) { // the hottest
			x.GPUTempC = g.TempC
		}
		used += g.MemUsedMiB
		total += g.MemTotalMiB
	}
	if nUtil > 0 {
		x.GPUUtil = util / float64(nUtil)
	}
	if total > 0 {
		x.GPUMemPct = used / total * 100
	}
	if h := snap.Host; h != nil && h.MemTotalKB > 0 && h.MemTotalKB >= h.MemAvailKB {
		x.HostMemUsedKB, x.HasHostMem = h.MemTotalKB-h.MemAvailKB, true
	}
	if p := snap.Proc; p != nil {
		x.ProcRSSKB, x.HasProcRSS = p.RSSKB, true
	}
	return x
}

func addUsage(a, b store.Usage) store.Usage {
	return store.Usage{
		GenTokens: a.GenTokens + b.GenTokens, PromptTokens: a.PromptTokens + b.PromptTokens,
		Requests: a.Requests + b.Requests, GPUEnergyWh: a.GPUEnergyWh + b.GPUEnergyWh,
		CPUEnergyWh: a.CPUEnergyWh + b.CPUEnergyWh, Cost: a.Cost + b.Cost,
	}
}

// fillHistory writes the cost, totals and database fields of snap.
func (c *Collector) fillHistory(snap *Snapshot) {
	h := &c.hist
	snap.GenTokensSinceStart = h.genTotal
	snap.DBReadOnly, snap.OneShot = c.cfg.DBReadOnly, c.cfg.OneShot
	if h.haveTotal {
		snap.Today, snap.Month, snap.AllTime = h.today, h.month, h.all
	}
	switch {
	case c.cfg.DBOpenErr != nil:
		snap.DBStatus = dbStatus(c.cfg.DBOpenErr)
	case c.cfg.Store != nil:
		err := c.cfg.Store.Err()
		if err == nil {
			err = h.readErr
		}
		if err == nil {
			h.mu.Lock()
			err = h.pruneErr
			h.mu.Unlock()
		}
		snap.DBStatus = dbStatus(err)
	}

	p := c.cfg.KWhPrice
	if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
		return
	}
	cs := Cost{Enabled: true, Currency: c.cfg.Currency, Price: p}
	cs.SinceStart = snap.EnergyWh / 1000 * p
	// mean power per source; a source with power but no integrated interval yet (the
	// first polls, --once, --frame) counts at its current draw
	meanW := 0.0
	switch {
	case c.gpuSecs > 0:
		meanW += c.energyWh * 3600 / c.gpuSecs
	case snap.HasGPUPower:
		meanW += snap.GPUPowerW
	}
	switch {
	case c.cpuSecs > 0:
		meanW += c.cpuEnergyWh * 3600 / c.cpuSecs
	case snap.HasCPUPower:
		meanW += snap.CPUPowerW
	}
	cs.PerDay = meanW / 1000 * 24 * p
	if h.genTotal >= minPerMTokens {
		cs.PerMTok = cs.SinceStart / float64(h.genTotal) * 1e6
	}
	snap.Cost = cs
}

// dbStatus is "" for nil, otherwise "off (<short reason>)".
func dbStatus(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errStoreTimeout):
		return "off (locked)"
	}
	return "off (" + store.Reason(err) + ")"
}
