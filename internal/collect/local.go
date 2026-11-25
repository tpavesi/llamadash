package collect

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"llamadash/internal/container"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/hwmon"
	"llamadash/internal/procstat"
	"llamadash/internal/stats"
)

const (
	procComm       = "llama-server"
	procRetryEvery = 10 * time.Second // between scans while the process is not running

	containerEvery   = 10 * time.Second // between container lookups
	containerTimeout = 5 * time.Second  // one lookup's own bound
	containerAge     = 30 * time.Second // a lookup seen in flight this long clears the value

	hwmonRetryEvery = 60 * time.Second // between reads while no sensor driver is loaded
	maxSeenFans     = 64               // fan identities remembered as having spun
)

// Fan is one motherboard fan as shown: it has reported more than 0 RPM at
// least once, in this run or (Config.KnownFans) an earlier one.
type Fan struct {
	Label, Chip string
	RPM         int
	Stopped     bool // reads 0 RPM after having spun
	Missing     bool // known, but its sensor input is not there (driver not loaded, chip gone): no RPM
}

// localResult is one round of host, process and hardware-monitor reads; the *Ran flags say which
// source was read.
type localResult struct {
	host    host.Info
	hostErr error
	hostRan bool
	proc    procstat.Info
	procErr error
	procRan bool
	hw      hwmon.Reading
	hwErr   error
	hwRan   bool
}

// localSensors is the state of the host, process, hardware-monitor and GPU-history sources.
type localSensors struct {
	hostRead func() (host.Info, error)
	procRead func(time.Time) (procstat.Info, error)
	hw       hwmonState

	busy      atomic.Bool // a host/process/hwmon read is running
	hostDead  bool
	procDead  bool
	procRetry time.Time // no process scan before this

	host   *host.Info
	hostAt time.Time
	proc   *procstat.Info
	procAt time.Time

	ct containerState

	hostCPUHist *stats.Ring
	gpuUtilHist *stats.Ring
	gpuMemHist  *stats.Ring
	gpuTempHist *stats.Ring
	hostSpan    *spanRing
	gpuSpan     *spanRing // the GPU rings, pushed together
}

func (c *Collector) initLocal() {
	l := &c.loc
	l.hostRead, l.procRead = c.cfg.Host, c.cfg.Proc
	if l.hostRead == nil {
		l.hostRead = host.NewReader("").Read
	}
	if l.procRead == nil {
		l.procRead = procstat.NewReader("", procComm).Read
	}
	l.hw.read, l.hw.spun, l.hw.onNew = c.cfg.Hwmon, map[string]bool{}, c.cfg.OnNewFans
	for _, id := range c.cfg.KnownFans {
		if !l.hw.spun[id] && len(l.hw.known) < maxSeenFans {
			l.hw.spun[id] = true
			l.hw.known = append(l.hw.known, id)
		}
	}
	if l.hw.read == nil {
		l.hw.read = hwmon.NewReader("").Read
	}
	l.ct.read, l.ct.port = c.cfg.Docker, hostPort(c.cfg.URL)
	if l.ct.read == nil {
		l.ct.read = container.Read
		if lookup := c.cfg.Container; lookup != nil {
			l.ct.read = lookupOnly(lookup)
		}
	}
	l.hostCPUHist, l.gpuUtilHist = stats.NewRing(histLen), stats.NewRing(histLen)
	l.gpuMemHist, l.gpuTempHist = stats.NewRing(histLen), stats.NewRing(histLen)
	l.hostSpan, l.gpuSpan = newSpanRing(histLen), newSpanRing(histLen)
}

// hostDue, procDue and hwmonDue report whether each source should be read in this poll.
func (c *Collector) hostDue() bool { return !c.loc.hostDead }
func (c *Collector) procDue(now time.Time) bool {
	return !c.loc.procDead && !now.Before(c.loc.procRetry)
}
func (c *Collector) hwmonDue(now time.Time) bool { return !now.Before(c.loc.hw.retry) }

// localDue reports whether any local read should start: never while one is still
// running, even if it ignores its context.
func (c *Collector) localDue(now time.Time) bool {
	return c.sourcesAllowed() && !c.loc.busy.Load() && (c.hostDue() || c.procDue(now) || c.hwmonDue(now))
}

// runLocal reads the due sources bounded by ctx. A read that outlives ctx keeps
// localDue false until it really returns (all three sources share the one
// goroutine, so a hung sysfs read blocks none of the polls and starts no more).
func (c *Collector) runLocal(ctx context.Context, now time.Time) localResult {
	wantHost, wantProc, wantHw := c.hostDue(), c.procDue(now), c.hwmonDue(now)
	c.loc.busy.Store(true)
	done := make(chan localResult, 1)
	go func() {
		var r localResult
		if wantHost {
			r.host, r.hostErr = c.loc.hostRead()
			r.hostRan = true
		}
		if wantProc {
			r.proc, r.procErr = c.loc.procRead(now)
			r.procRan = true
		}
		if wantHw {
			r.hw, r.hwErr = c.loc.hw.read()
			r.hwRan = true
		}
		c.loc.busy.Store(false)
		done <- r
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		return localResult{}
	}
}

// applyLocal folds a round of reads in. ErrUnavailable latches a source off; a
// missing process clears Proc and is retried every procRetryEvery; any other
// error keeps the last value (the renderer marks it stale via HostAt/ProcAt).
func (c *Collector) applyLocal(r localResult, now time.Time) {
	l := &c.loc
	if r.hostRan {
		switch {
		case errors.Is(r.hostErr, host.ErrUnavailable):
			l.hostDead, l.host = true, nil
		case r.hostErr == nil:
			h := r.host
			l.host, l.hostAt = &h, now
			if h.HasCPU {
				l.hostCPUHist.Push(h.CPUPct)
				l.hostSpan.push(now)
			}
		}
	}
	if r.procRan {
		switch {
		case errors.Is(r.procErr, procstat.ErrUnavailable):
			l.procDead, l.proc = true, nil
		case errors.Is(r.procErr, procstat.ErrNotFound):
			l.proc, l.procRetry = nil, now.Add(procRetryEvery)
		case r.procErr == nil:
			p := r.proc
			l.proc, l.procAt = &p, now
		}
	}
	if r.hwRan {
		l.hw.apply(r.hw, r.hwErr, now)
	}
}

// hwmonState is the fan and CPU temperature source.
type hwmonState struct {
	read  func() (hwmon.Reading, error)
	retry time.Time       // no read before this (the tree had nothing usable)
	spun  map[string]bool // fan identities that have reported more than 0 RPM; at most maxSeenFans
	known []string        // the same, in the order learned (seeds first)
	onNew func([]string)  // told the known list when it grows

	fans    []Fan // never changed in place: replaced by each reading
	tempC   float64
	hasTemp bool
	at      time.Time
}

// apply folds in a reading. Unavailable clears the values and waits
// hwmonRetryEvery (a driver may be loaded later); any other error keeps them.
// A fan is shown once it has spun, and from then on is flagged when it reads 0;
// a known fan absent from the reading (or every known fan, when there is no
// usable sensor at all) is shown as missing. A first spin is reported to onNew.
func (h *hwmonState) apply(r hwmon.Reading, err error, now time.Time) {
	switch {
	case errors.Is(err, hwmon.ErrUnavailable):
		h.hasTemp, h.tempC, h.retry = false, 0, now.Add(hwmonRetryEvery)
		if h.fans = h.missing(nil); h.fans != nil {
			h.at = now
		}
	case err == nil:
		var fans []Fan
		present := make(map[string]bool, len(r.Fans))
		added := false
		for _, f := range r.Fans {
			present[f.Key] = true
			if f.RPM > 0 && !h.spun[f.Key] && len(h.known) < maxSeenFans {
				h.spun[f.Key], h.known, added = true, append(h.known, f.Key), true
			}
			if h.spun[f.Key] {
				fans = append(fans, Fan{Label: f.Label, Chip: f.Chip, RPM: f.RPM, Stopped: f.RPM <= 0})
			}
		}
		fans = append(fans, h.missing(present)...)
		h.fans, h.tempC, h.hasTemp, h.at = fans, r.CPUTempC, r.HasCPUTemp, now
		if added && h.onNew != nil {
			h.onNew(slices.Clone(h.known))
		}
	}
}

// missing are the known fans not in present, labelled from their identity:
// "fan N" on the identity's chip.
func (h *hwmonState) missing(present map[string]bool) []Fan {
	var out []Fan
	for _, id := range h.known {
		if present[id] {
			continue
		}
		f := Fan{Label: id, Missing: true}
		if chip, n, ok := hwmon.ParseID(id); ok {
			f.Label, f.Chip = "fan "+strconv.Itoa(n), chip
		}
		out = append(out, f)
	}
	return out
}

// recordGPUHist pushes one sample per GPU result taken: mean utilisation, total
// VRAM used as a percentage of the total, and the hottest GPU.
func (c *Collector) recordGPUHist(gpus []gpu.GPU, now time.Time) {
	var utilSum, used, total, temp float64
	var nUtil, nTemp int
	for _, g := range gpus {
		if g.HasUtil {
			utilSum += g.UtilPct
			nUtil++
		}
		if g.HasTemp {
			temp = max(temp, g.TempC)
			nTemp++
		}
		used += g.MemUsedMiB
		total += g.MemTotalMiB
	}
	l := &c.loc
	if nUtil > 0 {
		l.gpuUtilHist.Push(utilSum / float64(nUtil))
	}
	if total > 0 {
		l.gpuMemHist.Push(used / total * 100)
	}
	if nTemp > 0 {
		l.gpuTempHist.Push(temp)
	}
	if nUtil > 0 || total > 0 || nTemp > 0 {
		l.gpuSpan.push(now)
	}
}

// fillLocal writes the host, process and GPU-history fields of snap.
func (c *Collector) fillLocal(snap *Snapshot) {
	l := &c.loc
	if l.host != nil {
		h := *l.host
		snap.Host, snap.HostAt = &h, l.hostAt
	}
	if l.proc != nil {
		p := *l.proc
		snap.Proc, snap.ProcAt = &p, l.procAt
	}
	if l.ct.info != nil {
		i := *l.ct.info
		snap.Container, snap.ContainerAt = &i, l.ct.at
	}
	if len(l.ct.list) > 0 {
		snap.Containers, snap.ContainersAt = slices.Clone(l.ct.list), l.ct.listAt
	}
	if len(l.hw.fans) > 0 || l.hw.hasTemp {
		snap.Fans, snap.HwmonAt = slices.Clone(l.hw.fans), l.hw.at
		snap.CPUTempC, snap.HasCPUTemp = l.hw.tempC, l.hw.hasTemp
	}
	snap.HostCPUHist, snap.HostCPUHistFrom, snap.HostCPUHistStarts = l.hostCPUHist.Values(), l.hostSpan.from(), l.hostSpan.values()
	snap.GPUHistFrom, snap.GPUHistStarts = l.gpuSpan.from(), l.gpuSpan.values()
	snap.GPUUtilHist, snap.GPUMemHist, snap.GPUTempHist = l.gpuUtilHist.Values(), l.gpuMemHist.Values(), l.gpuTempHist.Values()
}

// containerState is the container source. Lookups run on their own goroutine; the
// fields below mu are used by Poll only.
type containerState struct {
	read func(context.Context, int) (container.Overview, error)
	port int // host port published by the container; 0 -> source off

	mu     sync.Mutex // guards busy and latest, written by the lookup goroutine
	busy   bool       // a lookup is running (cleared only when it really returns)
	latest *containerResult

	dead     bool
	lastRun  time.Time // start of the last lookup
	busySeen time.Time // first poll that saw the current lookup in flight; zero when none
	info     *container.Info
	at       time.Time
	list     []container.Entry // never changed in place: replaced by each result
	listAt   time.Time
}

type containerResult struct {
	ov  container.Overview
	err error
	at  time.Time // the poll that started the lookup
}

// lookupOnly adapts a single-container lookup to the source: no list.
func lookupOnly(lookup func(context.Context, int) (container.Info, error)) func(context.Context, int) (container.Overview, error) {
	return func(ctx context.Context, port int) (container.Overview, error) {
		info, err := lookup(ctx, port)
		if err != nil && !errors.Is(err, container.ErrNotFound) {
			return container.Overview{}, err
		}
		return container.Overview{Llama: info, LlamaErr: err}, nil
	}
}

// hostPort is the port of rawURL, defaulting to 80 (http) or 443 (https); 0 when
// the URL has no usable port.
func hostPort(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	ps := u.Port()
	if ps == "" {
		switch u.Scheme {
		case "http":
			return 80
		case "https":
			return 443
		}
		return 0
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return p
}

// pollContainer folds in a lookup result delivered since the last poll, clears a
// value whose refresh keeps hanging, and starts a new lookup when one is due.
// It never waits for the lookup.
func (c *Collector) pollContainer(now time.Time) {
	ct := &c.loc.ct
	ct.mu.Lock()
	r, busy := ct.latest, ct.busy
	ct.latest = nil
	ct.mu.Unlock()
	if r != nil {
		ct.apply(r)
	}
	switch {
	case !busy:
		ct.busySeen = time.Time{}
	case ct.busySeen.IsZero():
		ct.busySeen = now
	case now.Sub(ct.busySeen) >= containerAge:
		ct.info, ct.list = nil, nil
	}
	if ct.dead || ct.port == 0 || busy || !c.sourcesAllowed() ||
		(!ct.lastRun.IsZero() && now.Sub(ct.lastRun) < containerEvery) {
		return
	}
	ct.lastRun, ct.busySeen = now, time.Time{}
	ct.mu.Lock()
	ct.busy = true
	ct.mu.Unlock()
	read, port := ct.read, ct.port
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
		ov, err := read(ctx, port)
		cancel()
		ct.mu.Lock()
		ct.latest = &containerResult{ov: ov, err: err, at: now}
		ct.busy = false
		ct.mu.Unlock()
	}()
}

// apply folds in a lookup result. A failed docker ps decides for the list and the
// llama container together: unavailable latches the source off, a daemon that is
// down clears both, any other error keeps both (their times show the age). After
// a good ps the list is replaced and the llama container follows its own lookup.
func (ct *containerState) apply(r *containerResult) {
	err := r.err
	if err == nil {
		ct.list, ct.listAt = r.ov.List, r.at
		err = r.ov.LlamaErr
	}
	switch {
	case errors.Is(err, container.ErrUnavailable):
		ct.dead, ct.info, ct.list = true, nil, nil
	case errors.Is(err, container.ErrDaemonDown):
		ct.info = nil
		if r.err != nil {
			ct.list = nil
		}
	case errors.Is(err, container.ErrNotFound):
		ct.info = nil
	case err == nil:
		i := r.ov.Llama
		ct.info, ct.at = &i, r.at
	} // any other error keeps the last value; ContainerAt shows its age
}
