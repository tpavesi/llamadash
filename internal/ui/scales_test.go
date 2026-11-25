package ui

import (
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"llamadash/internal/collect"
)

func TestRoundTop(t *testing.T) {
	cases := []struct {
		key     string
		v, want float64
	}{
		{"gen_rate", 0, 0}, {"gen_rate", -3, 0}, {"gen_rate", 37, 37}, {"gen_rate", 37.2, 38}, {"gen_rate", 87.5, 88},
		{"prompt_rate", 1400, 1400}, {"prompt_rate", 1400.01, 1401}, {"gen_rate", 10, 10}, {"gen_rate", 9.95, 10},
		{"gen_rate", 9.91, 10}, {"gen_rate", 7.12, 7.2}, {"gen_rate", 0.3, 0.3}, {"gen_rate", 0.0001, 0.1},
		{"power", 578.46, 580}, {"power", 580, 580}, {"power", 1001, 1010}, {"power", 3, 10}, {"power", 0, 0},
	}
	for _, c := range cases {
		if got := roundTop(c.key, c.v); got != c.want {
			t.Errorf("roundTop(%s, %v) = %v, want %v", c.key, c.v, got, c.want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := roundTop("gen_rate", v); got != 0 {
			t.Errorf("roundTop(%v) = %v", v, got)
		}
	}
}

// chartTop is the top label of the chart titled title.
func chartTop(t *testing.T, out, title string) string {
	t.Helper()
	c := panelContent(lines(out), title)
	if len(c) == 0 {
		t.Fatalf("no %q chart in\n%s", title, out)
	}
	return strings.TrimSpace(strings.Split(c[0], "│")[1])
}

// The power and token charts go to their top, not to the largest value drawn.
func TestChartsUseTheirTop(t *testing.T) {
	s := fullSnapshot(t)
	for i := range s.GenHist {
		s.GenHist[i] = 37
	}
	s.GenRate = 37
	o := opts(135, 120, s)
	out := stripANSI(Render(s, o))
	// nothing set: the largest value (the measured peak)
	if got := chartTop(t, out, "generation tok/s"); got != "37 tok/s" {
		t.Errorf("no top set: %q", got)
	}
	o.Scales = Scales{PowerW: 1000, GenTPS: 100, PromptTPS: 1500}
	out = stripANSI(Render(s, o))
	for title, want := range map[string]string{"generation tok/s": "100 tok/s", "power": "1000 W", "prompt tok/s": "1500 tok/s"} {
		if got := chartTop(t, out, title); got != want {
			t.Errorf("%s: top %q, want %q", title, got, want)
		}
	}
	// 37 of 100 fills 37% of the rows
	c := panelContent(lines(out), "generation tok/s")
	filled := 0
	for _, l := range c {
		if strings.Contains(l, "█") {
			filled++
		}
	}
	if want := int(math.Round(0.37 * float64(len(c)))); filled != want {
		t.Errorf("37 of 100 fills %d of %d rows, want %d", filled, len(c), want)
	}
	// a value above the top raises it to that peak, rounded up
	s.GenHist[5] = 129.4
	if got := chartTop(t, stripANSI(Render(s, o)), "generation tok/s"); got != "130 tok/s" {
		t.Errorf("above the top: %q", got)
	}
}

// A long range shares the top, and history above it raises it for the live bars too.
func TestTopSameForEveryRange(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	o.Scales = Scales{GenTPS: 100}
	o.Range, o.History, o.LongAt = Range24h, true, s.At
	o.Long = map[string][]float64{}
	for k, n := range chartPlotWidths(s, o) {
		o.Long[k] = make([]float64, n)
	}
	out := stripANSI(Render(s, o))
	if got := chartTop(t, out, "generation tok/s"); got != "100 tok/s" {
		t.Errorf("24h: %q", got)
	}
	o.Long["gen_rate"][3] = 230
	r := newRenderer(s, o)
	if got := r.top("gen_rate"); got != 230 {
		t.Errorf("history above the top: %v", got)
	}
	if got := chartTop(t, stripANSI(Render(s, o)), "generation tok/s"); got != "230 tok/s" {
		t.Errorf("24h raised: %q", got)
	}
	if r.top("gpu_util") != 0 {
		t.Error("a percent chart has a learned top")
	}
}

// The Throughput sparkline scales to the top, not to the largest sample.
func TestTrendUsesTop(t *testing.T) {
	r := newRenderer(collect.Snapshot{}, Options{NoColor: true})
	vals := []float64{50, 50, 50}
	if got := strings.TrimSpace(r.trend(vals, 3, rGen, 0)); got != "███" {
		t.Errorf("no top: %q", got)
	}
	if got := strings.TrimSpace(r.trend(vals, 3, rGen, 100)); got != "▅▅▅" {
		t.Errorf("top 100: %q", got)
	}
	a := newRenderer(collect.Snapshot{}, Options{NoColor: true, ASCII: true})
	if got := a.trend(vals, 30, rGen, 100); !strings.HasPrefix(got, strings.Repeat("#", 10)+".") { // 19 cells, half

		t.Errorf("ascii bar at half the top: %q", got)
	}
}

// The model keeps the tops for the session: raised by polls and history, never
// lowered, reported once per raise, and frozen on screen while paused.
func TestModelRaisesScales(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}}
	m := newSourceModel(t, src).(*Model)
	var got []Scales
	m.opts.Scales = Scales{PowerW: 1000}
	m.scales = m.opts.Scales
	m.onScales = func(s Scales) { got = append(got, s) }
	m.Update(tea.WindowSizeMsg{Width: 135, Height: 120})
	s := fullSnapshot(t)
	s.PowerHist = []float64{400, 500}
	s.PowerW = 500
	s.GenHist, s.GenRate = []float64{10, 37}, 37
	s.PromptHist, s.PromptRate = []float64{0, 0}, 0
	m.Update(pollMsg{snap: s})
	if m.scales != (Scales{PowerW: 1000, GenTPS: 37}) || len(got) != 1 || got[0] != m.scales {
		t.Fatalf("after the first poll: %+v, reported %v", m.scales, got)
	}
	m.Update(pollMsg{snap: s}) // nothing new: no report
	s.GenHist, s.GenRate, s.PowerW = []float64{5}, 5, 1203
	m.Update(pollMsg{snap: s}) // power above the PSU (to the next 10 W), gen lower: kept
	if m.scales != (Scales{PowerW: 1210, GenTPS: 37}) || len(got) != 2 {
		t.Fatalf("raise: %+v, reported %v", m.scales, got)
	}
	if !strings.Contains(chartTop(t, stripANSI(m.View().Content), "generation tok/s"), "37 tok/s") {
		t.Errorf("the top fell with the data")
	}
	// paused: the raise is kept and reported, the picture stays as it was
	m.Update(key("p"))
	s.GenHist, s.GenRate = []float64{90}, 90
	m.Update(pollMsg{snap: s})
	if m.scales.GenTPS != 90 || len(got) != 3 {
		t.Fatalf("paused: %+v, reported %d", m.scales, len(got))
	}
	if top := chartTop(t, stripANSI(m.View().Content), "generation tok/s"); top != "37 tok/s" {
		t.Errorf("paused picture changed: %q", top)
	}
	m.Update(key("p"))
	if top := chartTop(t, stripANSI(m.View().Content), "generation tok/s"); top != "90 tok/s" {
		t.Errorf("resumed: %q", top)
	}
	// history data raises the top too
	m.Update(longMsg{rng: Range1h, at: s.At, data: map[string][]float64{"prompt_rate": {math.NaN(), 700}}})
	if m.scales.PromptTPS != 700 || len(got) != 4 {
		t.Fatalf("history: %+v, reported %d", m.scales, len(got))
	}
}

func TestNewModelTakesScales(t *testing.T) {
	var got []Scales
	o := Options{Scales: Scales{GenTPS: 50}, OnScales: func(s Scales) { got = append(got, s) }}
	m := NewModel(collect.New(collect.Config{URL: "http://127.0.0.1:1", GPU: collect.GPUOff}), time.Second, o, nil).(*Model)
	if m.scales.GenTPS != 50 || m.opts.Scales.GenTPS != 50 || m.onScales == nil {
		t.Fatalf("%+v", m.scales)
	}
}

// The draw row and the plain text show the share of the power supply when its
// wattage is set.
func TestPSUShare(t *testing.T) {
	s := fullSnapshot(t)
	s.PowerW, s.PSUWatts = 578, 1000
	out := stripANSI(Render(s, opts(135, 120, s)))
	if c := panelContent(lines(out), "Energy"); len(c) == 0 || !strings.Contains(c[0], "578 W  of 1000 W psu (58%)") {
		t.Errorf("draw row: %q", c)
	}
	if p := RenderPlain(s); !strings.Contains(p, "power 578 W of 1000 W psu (gpu") {
		t.Errorf("plain:\n%s", p)
	}
	s.PSUWatts = 0
	if p := RenderPlain(s); strings.Contains(p, "psu") {
		t.Errorf("no psu set:\n%s", p)
	}
	if c := panelContent(lines(stripANSI(Render(s, opts(135, 120, s)))), "Energy"); strings.Contains(c[0], "psu") {
		t.Errorf("no psu set: %q", c[0])
	}
}

// An absurd reading (non-finite, or above what a top may be) never raises a top,
// so it is never shown, remembered or written.
func TestAbsurdReadingsIgnored(t *testing.T) {
	var sc Scales
	s := collect.Snapshot{GenRate: math.Inf(1), GenHist: []float64{1e12, 40}, PromptRate: math.NaN(),
		PowerW: 2e5, PowerHist: []float64{500}}
	if !sc.raise(s, map[string][]float64{"prompt_rate": {math.Inf(1), 3e6, 700}}) ||
		sc != (Scales{PowerW: 500, GenTPS: 40, PromptTPS: 700}) {
		t.Fatalf("%+v", sc)
	}
	r := newRenderer(s, Options{})
	if r.top("gen_rate") != 40 || r.top("power") != 500 {
		t.Fatalf("drawn tops %v %v", r.top("gen_rate"), r.top("power"))
	}
}
