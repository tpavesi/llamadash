package ui

import (
	"fmt"
	"math"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/store"
)

// Range is the time span the charts show: the live rings, or the database.
type Range int

const (
	Range5m Range = iota // live: one chart column per poll
	Range1h              // the database, one bucket mean per chart column
	Range24h
	Range7d
	nRanges
)

var rangeNames = [nRanges]string{"5m", "1h", "24h", "7d"}

var rangeSpans = [nRanges]time.Duration{5 * time.Minute, time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

func (r Range) String() string {
	if r < 0 || r >= nRanges {
		return fmt.Sprintf("Range(%d)", int(r))
	}
	return rangeNames[r]
}

// ParseRange parses "5m", "1h", "24h" or "7d".
func ParseRange(s string) (Range, error) {
	for i, n := range rangeNames {
		if s == n {
			return Range(i), nil
		}
	}
	return Range5m, fmt.Errorf("range %q is not one of 5m, 1h, 24h, 7d", s)
}

// long reports whether the range comes from the database.
func (r Range) long() bool { return r > Range5m && r < nRanges }

// next is the range the h key moves to.
func (r Range) next() Range { return (r + 1) % nRanges }

// refresh is how often a long range is fetched again.
func (r Range) refresh() time.Duration {
	if r == Range1h {
		return 10 * time.Second
	}
	return time.Minute
}

// SeriesSource is the history database; *store.Store satisfies it.
type SeriesSource interface {
	Series(metric string, from, to time.Time, buckets int) ([]store.Point, error)
}

// chartMetrics are the samples columns behind each chart; a chart with two
// columns shows their sum.
var chartMetrics = map[string][]string{
	"gen_rate": {"gen_rate"}, "power": {"gpu_w", "cpu_w"}, "gpu_util": {"gpu_util"},
	"prompt_rate": {"prompt_rate"}, "gpu_temp": {"gpu_temp"}, "gpu_mem_pct": {"gpu_mem_pct"},
	"host_cpu": {"host_cpu"},
}

// chartKeys are the chart keys in priority order.
var chartKeys = []string{"gen_rate", "power", "gpu_util", "prompt_rate", "gpu_temp", "gpu_mem_pct", "host_cpu"}

// fetchLong reads the range ending at to for each chart in widths (key -> plot
// columns): one value per column, NaN where a bucket has no data. It is
// synchronous and can take seconds; callers keep it off the UI thread.
func fetchLong(src SeriesSource, rng Range, to time.Time, widths map[string]int) (map[string][]float64, error) {
	from := to.Add(-rangeSpans[rng])
	out := make(map[string][]float64, len(widths))
	for _, key := range chartKeys {
		n := widths[key]
		if n <= 0 {
			continue
		}
		var sum []float64
		for _, metric := range chartMetrics[key] {
			pts, err := src.Series(metric, from, to, n)
			if err != nil {
				return nil, err
			}
			sum = addSeries(sum, bucketize(pts, from, to, n))
		}
		out[key] = sum
	}
	return out, nil
}

// bucketize places the store's points (each at its bucket's start, from + b*span/n
// in whole milliseconds) into n columns; columns without a point are NaN.
func bucketize(pts []store.Point, from, to time.Time, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	lo, span := from.UnixMilli(), to.UnixMilli()-from.UnixMilli()
	if span <= 0 {
		return out
	}
	for _, p := range pts {
		d := p.At.UnixMilli() - lo
		if d < 0 || d > span {
			continue
		}
		// the inverse of the store's floor(b*span/n): ceil(d*n/span)
		b := (d*int64(n) + span - 1) / span
		if b >= 0 && b < int64(n) {
			out[b] = p.V
		}
	}
	return out
}

// addSeries adds b to a column by column; a column is NaN only when both are.
func addSeries(a, b []float64) []float64 {
	if a == nil {
		return b
	}
	for i := range a {
		switch {
		case i >= len(b) || math.IsNaN(b[i]):
		case math.IsNaN(a[i]):
			a[i] = b[i]
		default:
			a[i] += b[i]
		}
	}
	return a
}

// LoadHistory fills o's long-range chart data for one frame (--frame): it fetches
// synchronously, bounded by timeout. No source, an error or a timeout leave the
// charts showing "history unavailable". The live range needs nothing.
func LoadHistory(src SeriesSource, s collect.Snapshot, o Options, timeout time.Duration) Options {
	if !o.Range.long() {
		return o
	}
	to := o.Now
	if to.IsZero() {
		to = time.Now()
	}
	o.LongAt, o.Long, o.LongErr = to, nil, true
	if src == nil {
		return o
	}
	type result struct {
		data map[string][]float64
		err  error
	}
	widths := chartPlotWidths(s, o)
	done := make(chan result, 1)
	go func() {
		d, err := fetchLong(src, o.Range, to, widths)
		done <- result{d, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-done:
		if r.err == nil {
			o.Long, o.LongErr = r.data, false
		}
	case <-t.C:
	}
	return o
}
