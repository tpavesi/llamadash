package ui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func clamp01(f float64) float64 {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	return min(f, 1)
}

// bar is a w-cell horizontal bar filled to frac (0..1).
func (r *renderer) bar(frac float64, w int, fill role, paint func(role, string) string) string {
	if w <= 0 {
		return ""
	}
	n := int(math.Round(clamp01(frac) * float64(w)))
	return paint(fill, strings.Repeat(r.g.full, n)) + paint(rTrack, strings.Repeat(r.g.empty, w-n))
}

func maxOf(vs []float64) float64 {
	m := 0.0
	for _, v := range vs {
		if v > m {
			m = v
		}
	}
	return m
}

// maxGauge caps every horizontal bar so bars read as gauges, not rules.
const maxGauge = 60

// trend is a sparkline of the last w samples, or with console/ASCII glyphs a bar
// of the latest value, followed by the largest sample (the peak). Both scale to
// top when it is > 0, else to the largest sample.
func (r *renderer) trend(vals []float64, w int, ro role, top float64) string {
	if w <= 0 {
		return ""
	}
	if r.g.spark == nil {
		latest, peakV := 0.0, maxOf(vals)
		if len(vals) > 0 {
			latest = vals[len(vals)-1]
		}
		m := peakV
		if top > 0 {
			m = top
		}
		frac := 0.0
		if m > 0 {
			frac = latest / m
		}
		peak := "  peak " + fmtRate(peakV)
		if w-width(peak) < 4 {
			return r.bar(frac, w, ro, r.paint)
		}
		return r.bar(frac, min(w-width(peak), maxGauge), ro, r.paint) + r.paint(rDim, peak)
	}
	vs := vals[max(0, len(vals)-w):]
	m := maxOf(vs)
	if top > 0 {
		m = top
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", w-len(vs)))
	for _, v := range vs {
		if v <= 0 || m <= 0 {
			b.WriteByte(' ')
			continue
		}
		i := int(math.Round(clamp01(v/m) * float64(len(r.g.spark)-1)))
		b.WriteString(r.g.spark[i])
	}
	return r.paint(ro, b.String())
}

// longLabelW is the y-axis label column of a long-range chart ("9999 tok/s "),
// fixed so the plot width, which sets the bucket count, is known before the data.
const longLabelW = 11

// liveGeom is the label column and plot width of a live chart w cells wide. The
// label column fits the largest value in the whole history (or the fixed axis
// max), so it does not jump while the ring moves: value, unit (at least as wide
// as "tok/s" so charts share the axis column), space.
func liveGeom(vals []float64, w int, unit string, fixed float64) (lw, plot int) {
	lw = max(width(fmtRate(max(maxOf(vals), fixed))), width(axisNum(fixed)), 4) + 1 + max(width(unit), 5) + 1
	if plot = w - lw - 1; plot < 4 { // no room for the axis labels
		return 0, max(w-1, 0)
	}
	return lw, plot
}

// longGeom is the label column and plot width of a long-range chart w cells wide.
func longGeom(w int) (lw, plot int) {
	if plot = w - longLabelW - 1; plot < 4 {
		return 0, max(w-1, 0)
	}
	return longLabelW, plot
}

// chart is a live column chart w cells wide and h rows tall: one sample per
// one-cell column, the newest at the right, as many of the newest samples as the
// plot has columns (liveShown). fixed > 0 is a fixed axis max; otherwise the axis
// goes to the largest drawn value.
func (r *renderer) chart(vals []float64, w, h int, unit string, ro role, fixed float64) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	lw, plot := liveGeom(vals, w, unit, fixed)
	return r.columns(vals[len(vals)-liveShown(vals, w, unit, fixed):], lw, plot, h, unit, ro, fixed)
}

// liveShown is how many of the newest samples a live chart w cells wide draws:
// one per plot column.
func liveShown(vals []float64, w int, unit string, fixed float64) int {
	_, plot := liveGeom(vals, w, unit, fixed)
	return min(len(vals), max(plot, 0))
}

// chartLong is a column chart of one value per column (the newest at the right)
// over a time axis row for rng, h rows in all; a NaN column has no data and shows
// a ░ on its bottom row.
func (r *renderer) chartLong(vals []float64, w, h int, unit string, ro role, fixed float64, rng Range) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	lw, plot := longGeom(w)
	if h < 2 {
		return r.columns(vals[max(0, len(vals)-plot):], lw, plot, h, unit, ro, fixed)
	}
	out := r.columns(vals[max(0, len(vals)-plot):], lw, plot, h-1, unit, ro, fixed)
	corner := ""
	if lw > 0 {
		corner = strings.Repeat(" ", lw)
	}
	return append(out, corner+r.paint(rBorder, r.g.bl)+r.paint(rDim, timeAxis(rng, plot)))
}

// axisLabels are each long range's start and middle labels.
var axisLabels = map[Range][2]string{Range1h: {"-1h", "-30m"}, Range24h: {"-24h", "-12h"}, Range7d: {"-7d", "-3.5d"}}

// timeAxis is the plot-wide label row of a long range: its start at the left,
// "now" at the right and, at 40 columns or more, the middle centred.
func timeAxis(rng Range, plot int) string {
	row := []rune(strings.Repeat(" ", plot))
	put := func(at int, s string) {
		for i, c := range []rune(s) {
			if at+i >= 0 && at+i < plot {
				row[at+i] = c
			}
		}
	}
	l := axisLabels[rng]
	if plot >= len(l[0])+1+3 {
		put(0, l[0])
	}
	if plot >= 40 {
		put(plot/2-len(l[1])/2, l[1])
	}
	put(plot-3, "now")
	return string(row)
}

// columns draws vs right-aligned in plot columns h rows tall using only full
// blocks and spaces, after a label column lw wide (0: none). The y axis goes from
// 0 to fixed when fixed > 0, else to the largest value, and is labelled at the top,
// at 0 and, on tall charts, at half height (a fixed axis only when a row is exactly
// half way); without any value only the axis is drawn. A NaN value has no data: a
// ░ on the bottom row.
func (r *renderer) columns(vs []float64, lw, plot, h int, unit string, ro role, fixed float64) []string {
	m, have := 0.0, false
	for _, v := range vs {
		if !math.IsNaN(v) {
			m, have = max(m, v), true
		}
	}
	if fixed > 0 {
		m = fixed
	}
	heights := make([]int, len(vs))
	for i, v := range vs {
		switch {
		case math.IsNaN(v):
			heights[i] = -1
		case m > 0 && v > 0:
			heights[i] = max(1, int(math.Round(clamp01(v/m)*float64(h))))
		}
	}
	out := make([]string, h)
	for row := 0; row < h; row++ {
		level := h - row // a column of height >= level fills this row
		var b strings.Builder
		run, kind := 0, 0 // a run of cells of one kind: 0 space, 1 block, 2 no data
		flush := func() {
			switch kind {
			case 0:
				b.WriteString(strings.Repeat(" ", run))
			case 1:
				b.WriteString(r.paint(ro, strings.Repeat(r.g.full, run)))
			case 2:
				b.WriteString(r.paint(rTrack, strings.Repeat(r.g.empty, run)))
			}
		}
		for _, ht := range heights {
			k := 0
			switch {
			case ht >= level:
				k = 1
			case ht < 0 && row == h-1:
				k = 2
			}
			if k != kind && run > 0 {
				flush()
				run = 0
			}
			kind = k
			run++
		}
		flush()
		label := ""
		switch {
		case lw == 0 || !have && fixed <= 0:
		case row == 0 && fixed > 0:
			label = axisNum(fixed) + " " + unit
		case row == 0:
			label = fmtRate(m) + " " + unit
		case row == h-1:
			label = "0"
		case h >= 8 && m > 0 && row == h-h/2: // a mid-scale label on tall charts
			switch {
			case fixed <= 0:
				label = fmtRate(m * float64(h/2) / float64(h))
			case h%2 == 0: // a fixed axis only where the row is exactly half way: 46.7 would read like data
				label = axisNum(fixed / 2)
			}
		}
		if lw > 0 {
			label += " "
		}
		out[row] = r.paint(rDim, padLeft(label, lw)) + r.paint(rBorder, r.g.v) +
			strings.Repeat(" ", max(0, plot-len(vs))) + b.String()
	}
	return out
}

// chartSpec is one chart of the grid.
type chartSpec struct {
	key, title, unit string
	ro               role
	live             []float64
	fixed            float64       // the axis max: 100 (percent, °C) or the learned top; 0: the largest value
	period           time.Duration // spacing of the live samples (when from is unknown)
	from             time.Time     // when the ring's oldest sample period began; zero: unknown
	starts           []time.Time   // when each live sample's period began; used when one per sample
	stale            string        // the live source's stale tag
}

// chartSpecs are the charts whose data source exists, in priority order.
func (r *renderer) chartSpecs() []chartSpec {
	s, iv := r.s, r.o.Interval
	gpuIv := max(iv, 2*time.Second) // the GPU query runs every 2 s at most
	var util, temp, mem bool
	for _, g := range s.GPUs {
		util, temp, mem = util || g.HasUtil, temp || g.HasTemp, mem || g.MemTotalMiB > 0
	}
	deg := r.g.deg + "C"
	out := []chartSpec{{key: "gen_rate", title: "generation tok/s", unit: "tok/s", ro: rGen, live: s.GenHist,
		fixed: r.top("gen_rate"), period: iv, from: s.GenHistFrom, starts: s.GenHistStarts, stale: r.serverStale()}}
	if s.HasPower {
		p := iv
		if !s.HasCPUPower { // GPU power alone is fresh once per GPU query
			p = gpuIv
		}
		out = append(out, chartSpec{key: "power", title: "power", unit: "W", ro: rPower, live: s.PowerHist,
			fixed: r.top("power"), period: p, from: s.PowerHistFrom, starts: s.PowerHistStarts, stale: r.energyStale()})
	}
	if util {
		out = append(out, chartSpec{key: "gpu_util", title: "GPU load", unit: "%", ro: rGood, live: s.GPUUtilHist, fixed: 100,
			period: gpuIv, from: s.GPUHistFrom, starts: s.GPUHistStarts, stale: r.gpuStale()})
	}
	out = append(out, chartSpec{key: "prompt_rate", title: "prompt tok/s", unit: "tok/s", ro: rPrompt, live: s.PromptHist,
		fixed: r.top("prompt_rate"), period: iv, from: s.GenHistFrom, starts: s.GenHistStarts, stale: r.serverStale()})
	if temp {
		out = append(out, chartSpec{key: "gpu_temp", title: "GPU temperature", unit: deg, ro: rValue, live: s.GPUTempHist, fixed: 100,
			period: gpuIv, from: s.GPUHistFrom, starts: s.GPUHistStarts, stale: r.gpuStale()})
	}
	if mem {
		out = append(out, chartSpec{key: "gpu_mem_pct", title: "VRAM %", unit: "%", ro: rMem, live: s.GPUMemHist, fixed: 100,
			period: gpuIv, from: s.GPUHistFrom, starts: s.GPUHistStarts, stale: r.gpuStale()})
	}
	if s.Host != nil && s.Host.HasCPU {
		out = append(out, chartSpec{key: "host_cpu", title: "host CPU %", unit: "%", ro: rGood, live: s.HostCPUHist, fixed: 100,
			period: iv, from: s.HostCPUHistFrom, starts: s.HostCPUHistStarts, stale: r.staleTag(s.HostAt, iv)})
	}
	return out
}

// chartPanel is the chart c as a panel iw cells wide inside and h rows tall. The
// tag says what span it covers: live, the span of the samples drawn ("last 2m":
// from the collector's timestamps, else samples x their spacing); from the
// database, the range and "avg per column" ("loading" until data for this plot
// width is there).
func (r *renderer) chartPanel(c chartSpec, iw, h int) panel {
	rng := r.o.Range
	if !rng.long() {
		tag := "no samples yet"
		if n := len(c.live); n > 0 {
			shown := max(liveShown(c.live, iw, c.unit, c.fixed), 1)
			span := time.Duration(shown) * c.period
			switch {
			case r.s.At.IsZero():
			case len(c.starts) == n: // the start of the oldest sample drawn
				span = r.s.At.Sub(c.starts[n-shown]).Round(time.Second)
			case shown == n && !c.from.IsZero():
				span = r.s.At.Sub(c.from).Round(time.Second)
			}
			tag = "last " + fmtSpan(span)
		}
		p := panel{title: c.title, tag: tag, tagRole: rDim, lines: r.chart(c.live, iw, h, c.unit, c.ro, c.fixed)}
		return r.staled(p, c.stale)
	}
	vals := r.o.Long[c.key]
	tag, tr := rng.String()+", avg per column", rDim
	_, plot := longGeom(iw)
	switch {
	case r.o.LongErr:
		tag, tr, vals = rng.String()+", history unavailable", rWarn, nil
	case r.o.LongAt.IsZero() || len(vals) != plot: // none yet, or fetched for another width
		tag, vals = rng.String()+", loading", nil
	}
	return panel{title: c.title, tag: tag, tagRole: tr, lines: r.chartLong(vals, iw, h, c.unit, c.ro, c.fixed, rng)}
}

// fmtSpan formats a chart span: 50s, 5m, 8m20s, 1h, 1h40m.
func fmtSpan(d time.Duration) string {
	s := int(max(d, 0).Seconds())
	switch {
	case s >= 3600 && s%3600 == 0:
		return fmt.Sprintf("%dh", s/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	case s >= 60 && s%60 == 0:
		return fmt.Sprintf("%dm", s/60)
	case s >= 60:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}
