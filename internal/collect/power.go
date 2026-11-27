package collect

import (
	"context"
	"errors"
	"time"

	"llamadash/internal/cpupower"
	"llamadash/internal/gpu"
)

const (
	gpuEvery     = 2 * time.Second
	gpuTimeout   = 2 * time.Second  // a GPU query's own bound, independent of the interval
	gpuPowerAge  = 30 * time.Second // GPU power older than this leaves the totals
	maxEnergyGap = 30 * time.Second // above the 10s maximum interval plus poll time
)

type gpuResult struct {
	gpus []gpu.GPU
	err  error
	at   time.Time // the poll that started the query
}

type cpuResult struct {
	watts, joules float64
	ok            bool
	err           error
}

// LocalSources reports whether this machine's own hardware may be used: the
// server is local, or --gpu was given, and --no-gpu was not.
func (c *Collector) LocalSources() bool { return c.sourcesAllowed() }

// sourcesAllowed reports whether local hardware (GPU, CPU power) may be read.
func (c *Collector) sourcesAllowed() bool {
	return c.cfg.GPU != GPUOff && (c.cfg.GPU != GPUAuto || c.gpuHost)
}

// gpuDue reports whether a GPU query should start in this poll: never while
// one is still running, even if it ignores its context.
func (c *Collector) gpuDue(now time.Time) bool {
	if c.gpuDead || !c.sourcesAllowed() {
		return false
	}
	c.gpuMu.Lock()
	busy := c.gpuBusy
	c.gpuMu.Unlock()
	if busy {
		return false
	}
	return c.lastQuery.IsZero() || now.Sub(c.lastQuery) >= gpuEvery
}

// cpuDue reports whether the CPU power source should be read in this poll.
func (c *Collector) cpuDue() bool {
	return !c.cpuDead && c.sourcesAllowed() && !c.cpuBusy.Load()
}

// startGPU runs the query in the background, bounded by gpuTimeout. Poll does not
// wait for it: the result lands in the latest slot, which the next Poll takes.
// The busy flag clears only when the query really returns.
func (c *Collector) startGPU(now time.Time) {
	c.lastQuery, c.busySeen = now, time.Time{}
	done := make(chan struct{})
	c.gpuMu.Lock()
	c.gpuBusy, c.gpuDone = true, done
	c.gpuMu.Unlock()
	query := c.gpuQuery
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gpuTimeout)
		g, err := query(ctx)
		cancel()
		c.gpuMu.Lock()
		c.gpuLatest = &gpuResult{gpus: g, err: err, at: now}
		c.gpuBusy = false
		c.gpuMu.Unlock()
		close(done)
	}()
}

// takeGPU returns the GPU result delivered since the last call, or nil.
func (c *Collector) takeGPU() *gpuResult {
	c.gpuMu.Lock()
	defer c.gpuMu.Unlock()
	r := c.gpuLatest
	c.gpuLatest = nil
	return r
}

// WaitGPU waits up to d for a GPU query in flight to return, so that the next
// Poll sees its result. It reports whether no query is running any more.
func (c *Collector) WaitGPU(d time.Duration) bool {
	c.gpuMu.Lock()
	done := c.gpuDone
	c.gpuMu.Unlock()
	if done == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// runCPU reads the CPU power source bounded by ctx. A read that outlives ctx
// keeps cpuDue false until it really returns.
func (c *Collector) runCPU(ctx context.Context, now time.Time) cpuResult {
	c.cpuBusy.Store(true)
	done := make(chan cpuResult, 1)
	go func() {
		w, j, ok, err := c.cpuRead(now)
		c.cpuBusy.Store(false)
		done <- cpuResult{watts: w, joules: j, ok: ok, err: err}
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return cpuResult{err: ctx.Err()}
	}
}

// applyGPU folds a GPU sample (taken at r.at) in and reports whether it carried fresh power.
func (c *Collector) applyGPU(r gpuResult) (fresh bool) {
	now := r.at
	c.gpuTakenAt, c.busySeen = now, time.Time{}
	switch {
	case errors.Is(r.err, gpu.ErrUnavailable):
		c.gpuDead = true
		c.gpus, c.havePower, c.powerW, c.havePrev = nil, false, 0, false
		return false
	case r.err != nil:
		return false // keep the last value; the renderer marks it stale via GPUAt
	}
	c.gpus, c.gpuAt = r.gpus, now
	c.recordGPUHist(r.gpus, now)
	if len(r.gpus) == 0 {
		c.gpus = nil
	}
	total, any := 0.0, false
	for _, g := range r.gpus {
		if g.HasPower {
			total += g.PowerW
			any = true
		}
	}
	c.havePower = any
	if !any {
		c.powerW, c.havePrev = 0, false
		return false
	}
	if c.havePrev {
		dt := now.Sub(c.prevAt)
		if dt > 0 && dt <= maxEnergyGap {
			c.energyWh += (c.prevW + total) / 2 * dt.Seconds() / 3600
			c.gpuSecs += dt.Seconds()
		}
	}
	c.powerW, c.prevW, c.prevAt, c.havePrev = total, total, now, true
	return true
}

// expireGPUPower drops GPU power from the totals once the queries have been
// failing for gpuPowerAge: the last result taken (good or not) is that much newer
// than the last good one. It is measured between query times, not against the
// poll, so a pause (suspend, stopped terminal) never expires a result that has
// only just been picked up. A query that never returns is failing too: it expires
// the power once polls have seen the same query in flight for gpuPowerAge, counted
// from the first poll that saw it busy (not from its start, which a pause can
// make arbitrarily old).
func (c *Collector) expireGPUPower(now time.Time) {
	c.gpuMu.Lock()
	busy := c.gpuBusy
	c.gpuMu.Unlock()
	hung := false
	switch {
	case !busy:
		c.busySeen = time.Time{}
	case c.busySeen.IsZero():
		c.busySeen = now
	default:
		hung = now.Sub(c.busySeen) >= gpuPowerAge
	}
	if c.havePower && (hung || c.gpuTakenAt.Sub(c.gpuAt) >= gpuPowerAge) {
		c.havePower, c.powerW, c.havePrev = false, 0, false
	}
}

// applyCPU folds a CPU power sample read at now in and reports whether it was a
// fresh reading. The joules of a reading cover the time since the previous read.
func (c *Collector) applyCPU(r cpuResult, now time.Time) (fresh bool) {
	switch {
	case errors.Is(r.err, cpupower.ErrUnavailable):
		c.cpuDead = true
		c.haveCPU, c.cpuW = false, 0
		return false
	case r.err != nil:
		return false // timeout: keep the last value
	}
	prev := c.cpuPrevAt
	c.cpuPrevAt = now
	if !r.ok {
		return false // first sample or reset: keep the last value
	}
	c.haveCPU, c.cpuW = true, r.watts
	c.cpuEnergyWh += r.joules / 3600
	if dt := now.Sub(prev); !prev.IsZero() && dt > 0 {
		c.cpuSecs += dt.Seconds()
	}
	return true
}

// fillPower writes the power fields of snap. Totals are GPU + CPU.
func (c *Collector) fillPower(snap *Snapshot, genRate float64) {
	snap.HasPower = c.havePower || c.haveCPU
	snap.HasGPUPower = c.havePower
	snap.GPUPowerW, snap.CPUPowerW, snap.HasCPUPower = c.powerW, c.cpuW, c.haveCPU
	snap.PowerW = c.powerW + c.cpuW
	snap.PSUWatts = c.cfg.PSUWatts
	snap.GPUEnergyWh, snap.CPUEnergyWh = c.energyWh, c.cpuEnergyWh
	snap.EnergyWh = c.energyWh + c.cpuEnergyWh
	snap.PowerHist, snap.PowerHistFrom, snap.PowerHistStarts = c.powerHist.Values(), c.powerSpan.from(), c.powerSpan.values()
	if snap.HasPower && genRate >= minRate {
		snap.JoulesPerToken = snap.PowerW / genRate
	}
}

// recordPower pushes the current total onto the history after a fresh sample.
func (c *Collector) recordPower(now time.Time) {
	if c.havePower || c.haveCPU {
		c.powerHist.Push(c.powerW + c.cpuW)
		c.powerSpan.push(now)
	}
}
