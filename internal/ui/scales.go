package ui

import (
	"math"
	"slices"
	"strconv"

	"llamadash/internal/collect"
)

// Scales are the tops of the charts whose top is learned: power, generation and
// prompt tokens per second (the percent and temperature charts are fixed at 100).
// A top only ever goes up: a value above it raises it to roundTop of the value,
// the measured peak rounded up a little. Zero means no top yet: the chart goes
// to roundTop of the largest value drawn.
type Scales struct {
	PowerW, GenTPS, PromptTPS float64
}

// scaleKeys are the chart keys with a learned top.
var scaleKeys = []string{"power", "gen_rate", "prompt_rate"}

// get returns the top of chart key (0 for a chart without a learned top).
func (sc Scales) get(key string) float64 {
	switch key {
	case "power":
		return sc.PowerW
	case "gen_rate":
		return sc.GenTPS
	case "prompt_rate":
		return sc.PromptTPS
	}
	return 0
}

func (sc *Scales) set(key string, v float64) {
	switch key {
	case "power":
		sc.PowerW = v
	case "gen_rate":
		sc.GenTPS = v
	case "prompt_rate":
		sc.PromptTPS = v
	}
}

// raise lifts each top to cover the snapshot's live values and the history data
// long (nil: none) and reports whether any top went up.
func (sc *Scales) raise(s collect.Snapshot, long map[string][]float64) bool {
	up := false
	for _, k := range scaleKeys {
		if n := roundTop(k, scaleMax(k, s, long[k])); n > sc.get(k) {
			sc.set(k, n)
			up = true
		}
	}
	return up
}

// Ceilings of a plausible reading: anything above (or not finite) is a fault in
// the source and never raises a top, so a top always stays one the settings
// reader accepts (at most 1e9) and one absurd sample cannot ruin a chart.
const (
	maxPowerTop = 100_000   // W, as for --psu-watts
	maxRateTop  = 1_000_000 // tok/s
)

// scaleMax is the largest plausible value of chart key in the snapshot (the
// latest reading and the live ring) and in the history columns long; NaN,
// infinite and implausibly large values are skipped.
func scaleMax(key string, s collect.Snapshot, long []float64) float64 {
	var ceiling float64
	var vals [][]float64
	switch key {
	case "power":
		ceiling, vals = maxPowerTop, [][]float64{{s.PowerW}, s.PowerHist, long}
	case "gen_rate":
		ceiling, vals = maxRateTop, [][]float64{{s.GenRate}, s.GenHist, long}
	case "prompt_rate":
		ceiling, vals = maxRateTop, [][]float64{{s.PromptRate}, s.PromptHist, long}
	default:
		return 0
	}
	m := 0.0
	for _, vs := range vals {
		for _, v := range vs {
			if v > m && v <= ceiling { // false for NaN; +Inf is above the ceiling
				m = v
			}
		}
	}
	return m
}

// roundTop is the top for peak v of chart key: power rounded up to the next
// 10 W; tokens per second rounded up to a whole number, or below 10 to one
// decimal. 0 for v <= 0 and for NaN or infinite v. (The small allowance keeps an
// exact 580 W or 0.3 tok/s from going up a step through float rounding.)
func roundTop(key string, v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0
	}
	const eps = 1e-9
	switch {
	case key == "power":
		return math.Ceil(v/10-eps) * 10
	case v >= 10:
		return math.Ceil(v - eps)
	}
	return math.Ceil(v*10-eps) / 10
}

// axisNum formats a fixed axis value: whole above 100, else without trailing zeros.
func axisNum(v float64) string {
	if v >= 100 {
		return strconv.FormatFloat(math.Round(v), 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// top is the top of chart key on this frame: the learned top, raised to cover
// every value of that metric drawn anywhere on it (the live readings and, on a
// long range, the history columns), so every chart and bar of the metric shares
// it; 0 for a chart without a learned top.
func (r *renderer) top(key string) float64 {
	if !slices.Contains(scaleKeys, key) {
		return 0
	}
	var long []float64
	if r.o.Range.long() {
		long = r.o.Long[key]
	}
	return max(r.o.Scales.get(key), roundTop(key, scaleMax(key, r.s, long)))
}
