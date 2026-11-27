package ui

import (
	"math"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/container"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
)

// Level is the worst state on the dashboard.
type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelBad
)

func (l Level) String() string {
	switch {
	case l >= LevelBad:
		return "bad"
	case l == LevelWarn:
		return "warn"
	}
	return "ok"
}

// Status is the level of s as the full dashboard would colour it. It uses
// o.Now and o.Interval (for stale tags) and nothing else from o.
func Status(s collect.Snapshot, o Options) Level {
	o = Options{Now: o.Now, Interval: o.Interval}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	return newRenderer(s, o).level()
}

// The limits below are shared by the panels and by level(), so the screen and
// the status cannot drift apart.
const (
	gpuTempWarn, gpuTempBad = 80, 90
	cpuTempWarn, cpuTempBad = 85, 95
	vramWarn, hostMemWarn   = 0.95, 0.9
	drawWarnPct, drawBadPct = 80, 95
)

// tempRole is the colour of a temperature, decided on the value shown: 79.6
// shows as 80 and is amber.
func tempRole(c, warn, bad float64) role {
	switch t := math.Round(c); {
	case t >= bad:
		return rBad
	case t >= warn:
		return rWarn
	}
	return rValue
}

// vramFrac is the share of a GPU's memory in use, and whether that is a warning.
func vramFrac(g gpu.GPU) (frac float64, high bool) {
	if g.MemTotalMiB > 0 {
		frac = g.MemUsedMiB / g.MemTotalMiB
	}
	return frac, frac >= vramWarn
}

// hostMem is the host's memory in use, its share, and whether that is a warning.
func hostMem(h host.Info) (usedKB uint64, frac float64, high bool) {
	usedKB = h.MemTotalKB - min(h.MemAvailKB, h.MemTotalKB)
	if h.MemTotalKB > 0 {
		frac = float64(usedKB) / float64(h.MemTotalKB)
	}
	return usedKB, frac, frac >= hostMemWarn
}

// healthOf is the header's health text and its colour.
func healthOf(s collect.Snapshot) (string, role) {
	switch {
	case s.At.IsZero():
		return "connecting", rWarn
	case !s.Reachable:
		return "unreachable", rBad
	case s.Loading:
		return "loading model", rWarn
	case s.Health.Status != "" && s.Health.Status != "ok":
		return ascii(s.Health.Status), rWarn
	}
	return "ok", rGood
}

// ctStateRole is the colour of the server container's state.
func ctStateRole(state string) role {
	if state != "running" {
		return rBad
	}
	return rGood
}

// ctHealthRole is the colour of the server container's healthcheck result.
func ctHealthRole(health string) role {
	switch health {
	case "":
		return rDim
	case "healthy":
		return rGood
	case "unhealthy":
		return rBad
	case "starting":
		return rWarn
	}
	return rValue
}

// ctLevelRole is the colour of a container's light in the Containers panel.
func ctLevelRole(l container.Level) role {
	switch l {
	case container.LevelOK:
		return rGood
	case container.LevelStarting:
		return rWarn
	case container.LevelFailed:
		return rBad
	}
	return rDim
}

// powerRole is the colour of the total draw: its share of the power supply's
// wattage as shown (a whole percentage), amber from 80%, red from 95%. Without
// a wattage there is nothing to measure against: the plain value colour.
func powerRole(powerW, psuWatts float64) role {
	if psuWatts <= 0 {
		return rValue
	}
	switch pct := math.Round(powerW / psuWatts * 100); {
	case pct >= drawBadPct:
		return rBad
	case pct >= drawWarnPct:
		return rWarn
	}
	return rGood
}

// hostStale is the stale tag of the Host panel: the fan and temperature
// readings share it.
func (r *renderer) hostStale() string {
	at := r.s.HostAt
	if (len(r.s.Fans) > 0 || r.s.HasCPUTemp) && r.s.HwmonAt.Before(at) {
		at = r.s.HwmonAt
	}
	return r.staleTag(at, r.o.Interval)
}

func (r *renderer) procStale() string { return r.staleTag(r.s.ProcAt, r.o.Interval) }

// containerStale and containersStale: the lookups run every containerEvery at most.
func (r *renderer) containerStale() string {
	return r.staleTag(r.s.ContainerAt, max(r.o.Interval, containerEvery))
}

func (r *renderer) containersStale() string {
	return r.staleTag(r.s.ContainersAt, max(r.o.Interval, containerEvery))
}

// level is the worst warning colour of every panel the snapshot has data for,
// whether or not it fits the screen. A stale panel is a warning and its dimmed
// content counts for nothing. Not counted, though coloured on screen: a missing
// fan, a slot's context bar (a full context is normal use), and "history
// unavailable" (it depends on the range one viewer chose).
func (r *renderer) level() Level {
	s := r.s
	worst := LevelOK
	add := func(ro role) {
		switch ro {
		case rBad:
			worst = LevelBad
		case rWarn:
			worst = max(worst, LevelWarn)
		}
	}
	// panel counts the stale tag, or when fresh what live adds
	panel := func(stale string, live func()) {
		if stale != "" {
			add(rWarn)
			return
		}
		live()
	}
	_, hr := healthOf(s)
	add(hr)
	if s.DBStatus != "" {
		add(rWarn)
	}
	panel(r.serverStale(), func() { // Requests, Throughput
		if s.SlotsHint != "" {
			add(rWarn)
		}
	})
	if len(s.GPUs) > 0 {
		panel(r.gpuStale(), func() {
			for _, g := range s.GPUs {
				if g.HasTemp {
					add(tempRole(g.TempC, gpuTempWarn, gpuTempBad))
				}
				if _, high := vramFrac(g); high {
					add(rWarn)
				}
			}
		})
	}
	if s.Host != nil {
		panel(r.hostStale(), func() {
			if _, _, high := hostMem(*s.Host); high {
				add(rWarn)
			}
			if s.HasCPUTemp {
				add(tempRole(s.CPUTempC, cpuTempWarn, cpuTempBad))
			}
			for _, f := range s.Fans {
				if f.Stopped {
					add(rBad)
				}
			}
		})
	}
	if s.Proc != nil {
		panel(r.procStale(), func() {})
	}
	if c := s.Container; c != nil {
		panel(r.containerStale(), func() {
			add(ctStateRole(ascii(c.State)))
			add(ctHealthRole(c.Health))
		})
	}
	if len(s.Containers) > 0 {
		panel(r.containersStale(), func() {
			for _, it := range ctItems(s) {
				add(ctLevelRole(it.level))
			}
		})
	}
	if s.HasPower {
		panel(r.energyStale(), func() { add(powerRole(s.PowerW, s.PSUWatts)) })
	}
	return worst
}
