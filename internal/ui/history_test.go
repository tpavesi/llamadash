package ui

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"llamadash/internal/collect"
	"llamadash/internal/store"
)

func TestParseRange(t *testing.T) {
	for s, want := range map[string]Range{"5m": Range5m, "1h": Range1h, "24h": Range24h, "7d": Range7d} {
		got, err := ParseRange(s)
		if err != nil || got != want || got.String() != s {
			t.Errorf("ParseRange(%q) = %v (%q), %v", s, got, got.String(), err)
		}
	}
	for _, bad := range []string{"2h", "", "5M", "1d", " 1h"} {
		if _, err := ParseRange(bad); err == nil {
			t.Errorf("ParseRange(%q) accepted", bad)
		}
	}
}

// seriesCall is one Series call a fakeSource saw.
type seriesCall struct {
	metric   string
	from, to time.Time
	buckets  int
}

// fakeSource answers Series with a constant value per metric in every bucket
// (metrics not in vals have no data). With block set each call waits on it.
type fakeSource struct {
	mu    sync.Mutex
	calls []seriesCall
	vals  map[string]float64
	err   error
	block chan struct{}
}

func (f *fakeSource) Series(metric string, from, to time.Time, n int) ([]store.Point, error) {
	f.mu.Lock()
	f.calls = append(f.calls, seriesCall{metric, from, to, n})
	block, err := f.block, f.err
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	v, ok := f.vals[metric]
	if !ok {
		return nil, nil
	}
	lo, span := from.UnixMilli(), to.UnixMilli()-from.UnixMilli()
	out := make([]store.Point, n)
	for b := range out { // the store's bucket starts
		out[b] = store.Point{At: time.UnixMilli(lo + int64(b)*span/int64(n)), V: v}
	}
	return out, nil
}

func (f *fakeSource) seen() []seriesCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seriesCall(nil), f.calls...)
}

// A column without data shows one ░ on the bottom row; idle (data = 0) is an
// empty column. The last row is the time axis.
func TestLongRangeGapsDrawnEmpty(t *testing.T) {
	s := fullSnapshot(t)
	r := newRenderer(s, opts(135, 120, s))
	nan := math.NaN()
	out := r.chartLong([]float64{nan, 0, 10, nan, 20}, longLabelW+1+5, 5, "tok/s", rGen, 0, Range1h)
	if len(out) != 5 {
		t.Fatalf("%d rows", len(out))
	}
	plot := func(row int) string { // the 5 plot cells right of the y axis
		l := []rune(stripANSI(out[row]))
		return string(l[len(l)-5:])
	}
	if got := plot(3); got != "░ █░█" {
		t.Errorf("bottom row %q, want \"░ █░█\"", got)
	}
	if got := plot(0); got != "    █" {
		t.Errorf("top row %q, want \"    █\"", got)
	}
	if top := stripANSI(out[0]); !strings.HasPrefix(strings.TrimSpace(top), "20.0 tok/s") {
		t.Errorf("y axis max is not the max of the plotted values: %q", top)
	}
	if axis := stripANSI(out[4]); strings.Contains(axis, "█") || !strings.Contains(axis, "now") {
		t.Errorf("no time axis row: %q", axis)
	}
}

// The time axis under a long-range chart: the range start at the left, "now" at
// the right edge, a middle label when the plot is at least 40 columns wide.
func TestLongRangeTimeAxis(t *testing.T) {
	s := fullSnapshot(t)
	r := newRenderer(s, opts(135, 120, s))
	for _, c := range []struct {
		rng        Range
		plot       int
		start, mid string
	}{
		{Range1h, 51, "-1h", "-30m"}, {Range24h, 51, "-24h", "-12h"}, {Range7d, 84, "-7d", "-3.5d"},
		{Range24h, 39, "-24h", ""},
	} {
		out := r.chartLong(nil, longLabelW+1+c.plot, 6, "tok/s", rGen, 0, c.rng)
		l := []rune(stripANSI(out[len(out)-1]))
		axis := string(l[len(l)-c.plot:])
		if !strings.HasPrefix(axis, c.start) || !strings.HasSuffix(axis, "now") {
			t.Errorf("%v %d: axis %q", c.rng, c.plot, axis)
		}
		if c.mid != "" {
			if i := strings.Index(axis, c.mid); i < 0 || absInt(len([]rune(axis[:i]))+len(c.mid)/2-c.plot/2) > 1 {
				t.Errorf("%v %d: middle label %q not centred in %q", c.rng, c.plot, c.mid, axis)
			}
		} else if strings.Count(axis, "-") != 1 {
			t.Errorf("%v %d: a middle label on a narrow chart: %q", c.rng, c.plot, axis)
		}
	}
	// live charts have no axis row: every row carries the y axis
	for _, l := range r.chart(hist(30), 60, 6, "tok/s", rGen, 0) {
		if !strings.Contains(stripANSI(l), "│") {
			t.Errorf("live row without the y axis: %q", stripANSI(l))
		}
	}
}

func absInt(v int) int { return max(v, -v) }

// A live chart draws one sample per one-cell column: the newest samples, as
// many as the plot has columns, never averaged or stretched.
func TestLiveOneSamplePerColumn(t *testing.T) {
	r := newRenderer(collect.Snapshot{}, Options{NoColor: true})
	vals := make([]float64, 300)
	for i := range vals {
		vals[i] = float64(i % 2 * 10) // 0, 10, 0, 10, ...: averaging would flatten it
	}
	lw, plot := liveGeom(vals, 60, "tok/s", 10)
	out := r.chart(vals, 60, 4, "tok/s", rGen, 10)
	bottom := []rune(out[3])[lw+1:]
	if len(bottom) != plot || liveShown(vals, 60, "tok/s", 10) != plot {
		t.Fatalf("plot %d, row %d cells, shown %d", plot, len(bottom), liveShown(vals, 60, "tok/s", 10))
	}
	for i, c := range bottom { // the newest sample (index 299, 10) at the right
		want := ' '
		if (len(bottom)-1-i)%2 == 0 {
			want = '█'
		}
		if c != want {
			t.Fatalf("column %d is %q in %q", i, c, string(bottom))
		}
	}
}

// The live title states the span of the samples drawn (from the collector's
// timestamps), and fewer samples than columns are drawn right-aligned, one per
// column.
func TestLiveTitleSpan(t *testing.T) {
	s := fullSnapshot(t)
	s.GenHist, s.GenHistStarts = make([]float64, 300), make([]time.Time, 300)
	for i := range s.GenHist {
		s.GenHist[i] = 40
		s.GenHistStarts[i] = s.At.Add(-time.Duration(300-i) * 2 * time.Second) // polls 2 s apart
	}
	s.GenHistFrom = s.GenHistStarts[0]
	o := opts(135, 120, s)
	out := stripANSI(Render(s, o))
	// 135 columns: two charts per row, 51 plot columns each, so the newest 51
	// samples, 102 s
	if title := panelTitle(out, "generation tok/s"); !strings.Contains(title, "last 1m42s ") {
		t.Errorf("300 samples at 2s: %q", title)
	}
	if title := panelTitle(stripANSI(Render(s, opts(160, 120, s))), "generation tok/s"); !strings.Contains(title, "last 2m08s ") {
		t.Errorf("160 columns: 64 samples at 2s: %q", title)
	}
	s.GenHistStarts = nil // no timestamps: samples x the poll interval
	if title := panelTitle(stripANSI(Render(s, o)), "generation tok/s"); !strings.Contains(title, "last 51s ") {
		t.Errorf("no timestamps: %q", title)
	}
	s.GenHist, s.GenHistFrom = s.GenHist[:20], s.At.Add(-20*time.Second)
	out = stripANSI(Render(s, opts(135, 120, s)))
	if title := panelTitle(out, "generation tok/s"); !strings.Contains(title, "last 20s ") {
		t.Errorf("20 samples at 1s: %q", title)
	}
	c := panelContent(lines(out), "generation tok/s")
	bottom := strings.TrimSuffix(strings.TrimRight(c[len(c)-1], " │"), " ")
	if n := strings.Count(bottom, "█"); n != 20 || !strings.HasSuffix(bottom, strings.Repeat("█", 20)) {
		t.Errorf("20 samples: %d columns, not right-aligned: %q", n, bottom)
	}
}

// Percent and temperature charts use a fixed 0-100 axis.
func TestFixedAxis(t *testing.T) {
	s := fullSnapshot(t)
	for i := range s.GPUMemHist {
		s.GPUMemHist[i] = 92
	}
	out := stripANSI(Render(s, opts(135, 120, s)))
	c := panelContent(lines(out), "VRAM %")
	if len(c) < 10 || !strings.Contains(c[0], "100 %") {
		t.Fatalf("VRAM chart: %q", c)
	}
	filled := 0
	for _, l := range c {
		if strings.Contains(l, "█") {
			filled++
		}
	}
	if want := int(math.Round(0.92 * float64(len(c)))); filled != want {
		t.Errorf("92%% fills %d of %d rows, want %d", filled, len(c), want)
	}
	if c := panelContent(lines(out), "GPU temperature"); !strings.Contains(c[0], "100 °C") {
		t.Errorf("temperature axis: %q", c[0])
	}
	// a rate chart goes to its learned top: here its measured peak, 88
	if c := panelContent(lines(out), "generation tok/s"); !strings.Contains(c[0], " 88 tok/s") {
		t.Errorf("rate chart top: %q", c[0])
	}
}

// A fixed 0-100 axis has no mid label (46.7 reads like a data value) unless its row
// is exactly half way, where it says 50; a data-scaled axis keeps its mid label.
func TestFixedAxisMidLabel(t *testing.T) {
	r := newRenderer(collect.Snapshot{}, Options{NoColor: true})
	vals := []float64{20, 40, 60}
	label := func(out []string, row int) string { return strings.TrimSpace(strings.Split(out[row], "│")[0]) }
	for _, h := range []int{8, 9, 15, 16} {
		out := r.columns(vals, 11, 10, h, "%", rGood, 100)
		mid := h - h/2
		want := ""
		if h%2 == 0 {
			want = "50"
		}
		if got := label(out, mid); got != want {
			t.Errorf("fixed, %d rows: mid label %q, want %q", h, got, want)
		}
		for row := 1; row < h-1; row++ {
			if row != mid && label(out, row) != "" {
				t.Errorf("fixed, %d rows: label %q on row %d", h, label(out, row), row)
			}
		}
		if got := label(r.columns(vals, 11, 10, h, "%", rGood, 0), mid); got == "" {
			t.Errorf("data axis, %d rows: no mid label", h)
		}
	}
}

// TestBucketsFromStore maps the store's bucket starts back to their columns,
// including the integer rounding of At = from + b*span/n.
func TestBucketsFromStore(t *testing.T) {
	to := time.UnixMilli(1_790_000_000_123)
	from := to.Add(-time.Hour)
	for _, n := range []int{1, 7, 50, 51, 84, 333} {
		var pts []store.Point
		lo, span := from.UnixMilli(), to.UnixMilli()-from.UnixMilli()
		for b := 0; b < n; b += 2 { // every other bucket
			pts = append(pts, store.Point{At: time.UnixMilli(lo + int64(b)*span/int64(n)), V: float64(b + 1)})
		}
		got := bucketize(pts, from, to, n)
		for b, v := range got {
			if b%2 == 0 && v != float64(b+1) || b%2 == 1 && !math.IsNaN(v) {
				t.Fatalf("n=%d bucket %d = %v", n, b, v)
			}
		}
	}
}

// The power chart sums gpu_w and cpu_w per column: a column with only one of them
// is that one, a column with neither is no data (not 0 W).
func TestLongRangePowerMissingSource(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	to := t0
	from := to.Add(-time.Hour)
	nan := math.NaN()
	for i, x := range []store.Sample{
		{GPUPowerW: 300, CPUPowerW: nan}, {GPUPowerW: nan, CPUPowerW: 100}, {GPUPowerW: nan, CPUPowerW: nan}, {GPUPowerW: 200, CPUPowerW: 50},
	} {
		x.At = from.Add(time.Duration(i)*15*time.Minute + time.Minute) // one sample per 15-minute column
		st.AddSample(x)
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := fetchLong(st, Range1h, to, map[string]int{"power": 4})
	if err != nil {
		t.Fatal(err)
	}
	p := got["power"]
	if len(p) != 4 || p[0] != 300 || p[1] != 100 || !math.IsNaN(p[2]) || p[3] != 250 {
		t.Fatalf("power columns %v, want [300 100 NaN 250]", p)
	}
}

func TestLongRangePowerIsSum(t *testing.T) {
	// 900 + 300 = 1200 W raises the top to 1200 W (900 alone would make it 900 W)
	src := &fakeSource{vals: map[string]float64{"gpu_w": 900, "cpu_w": 300, "gen_rate": 50}}
	m := longModel(t, src, 135, 120)
	m = pressAndFetch(t, m, "h")
	c := panelContent(lines(stripANSI(m.View().Content)), "power")
	if len(c) == 0 || !strings.Contains(c[0], "1200 W") {
		t.Fatalf("power chart top label: %q", c)
	}
}

// longModel is a settled model of size w x h with the full snapshot and src.
func longModel(t *testing.T, src SeriesSource, w, h int) tea.Model {
	t.Helper()
	m := newSourceModel(t, src)
	m, _ = update(m, tea.WindowSizeMsg{Width: w, Height: h})
	m, _ = update(m, pollMsg{snap: fullSnapshot(t)})
	return m
}

// pressAndFetch presses k and delivers the history fetch it starts.
func pressAndFetch(t *testing.T, m tea.Model, k string) tea.Model {
	t.Helper()
	m, cmd := update(m, key(k))
	msg := findLong(cmd)
	if msg == nil {
		t.Fatalf("%s: no history fetch", k)
	}
	m, _ = update(m, *msg)
	return m
}

// findLong runs cmd (and the commands of a batch) and returns the history result.
// Models from newSourceModel fire their ticks at once, so nothing here waits.
func findLong(cmd tea.Cmd) *longMsg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		switch msg := msg.(type) {
		case longMsg:
			return &msg
		case tea.BatchMsg:
			for _, c := range msg {
				if l := findLong(c); l != nil {
					return l
				}
			}
		}
	case <-time.After(5 * time.Second): // a fetch that hangs: fail, do not hang the suite
	}
	return nil
}

var errBusy = errors.New("database is locked")
