package ui

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"llamadash/internal/collect"
	"llamadash/internal/fakeserver"
	"llamadash/internal/gpu"
	"llamadash/internal/llama"
	"llamadash/internal/stats"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;:]*[A-Za-z]")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func oneGPU(t *testing.T) []gpu.GPU {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "nvidia-smi", "one_gpu.csv"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gpu.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// hist returns n plausible non-zero samples.
func hist(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = 30 + float64(i%17)*2
	}
	return out
}

// busy is the fixture snapshot plus one GPU, power and n history samples.
func busy(t *testing.T, n int) collect.Snapshot {
	s := fixtureSnapshot(t)
	s.GPUs, s.GPUAt = oneGPU(t), s.At
	s.GenHist, s.PromptHist = hist(n), hist(n)
	s.HasPower, s.PowerW, s.HasGPUPower, s.GPUPowerW, s.PowerHist = true, 412, true, 285, hist(n)
	s.HasCPUPower, s.CPUPowerW = true, 127
	s.EnergyWh, s.JoulesPerToken = 420, 7.5
	// the rings' spans as the collector would stamp them: n polls 1 s apart
	s.GenHistFrom, s.PowerHistFrom = s.At.Add(-time.Duration(n)*time.Second), s.At.Add(-time.Duration(n)*time.Second)
	s.GPUHistFrom, s.HostCPUHistFrom = time.Time{}, time.Time{}
	return s
}

func opts(w, h int, s collect.Snapshot) Options {
	return Options{Width: w, Height: h, Interval: time.Second, Now: s.At}
}

func idleSnapshot() collect.Snapshot {
	return collect.Snapshot{At: t0, SlotsAt: t0, Reachable: true, URL: "http://localhost:8080",
		Health: llama.Health{Status: "ok"}, Props: &llama.Props{TotalSlots: 1, NCtx: 4096, ModelAlias: "m"},
		Slots: []stats.SlotView{{Slot: llama.Slot{ID: 0, NCtx: 4096}}}}
}

func unreachableSnapshot() collect.Snapshot {
	return collect.Snapshot{At: t0.Add(5 * time.Second), URL: "http://10.9.8.7:8011", UnreachableSince: t0}
}

func checkFits(t *testing.T, name string, out string, o Options) {
	t.Helper()
	ls := lines(out)
	if o.Height > 0 && len(ls) > o.Height {
		t.Errorf("%s %dx%d: %d lines", name, o.Width, o.Height, len(ls))
	}
	for i, l := range ls {
		if w := lipgloss.Width(l); w > max(o.Width, 0) {
			t.Errorf("%s %dx%d: line %d is %d wide: %q", name, o.Width, o.Height, i, w, stripANSI(l))
		}
	}
}

func TestRenderFitsWidth(t *testing.T) {
	snaps := map[string]collect.Snapshot{
		"busy": busy(t, 300), "idle": idleSnapshot(), "unreachable": unreachableSnapshot(), "empty": {},
	}
	for name, s := range snaps {
		for _, w := range []int{0, 1, 20, 40, 79, 80, 120, 300} {
			for _, h := range []int{0, 1, 8, 24, 60} {
				for _, mode := range []Options{{}, {Color16: true}, {ASCII: true}, {NoColor: true}, {Paused: true}} {
					o := opts(w, h, s)
					o.Color16, o.ASCII, o.NoColor, o.Paused = mode.Color16, mode.ASCII, mode.NoColor, mode.Paused
					checkFits(t, name, Render(s, o), o)
				}
			}
		}
	}
}

// titleLine returns the index of the first line containing title, or -1.
func titleLine(ls []string, title string) int {
	for i, l := range ls {
		if strings.Contains(l, title) {
			return i
		}
	}
	return -1
}

// historyBody returns the lines between the generation chart's title line and its bottom border.
func historyBody(ls []string) []string {
	i := titleLine(ls, "generation tok/s")
	if i < 0 {
		return nil
	}
	var body []string
	for _, l := range ls[i+1:] {
		if strings.HasPrefix(l, "└") || strings.HasPrefix(l, "+") {
			break
		}
		body = append(body, l)
	}
	return body
}

func TestRenderPortrait(t *testing.T) {
	s := busy(t, 60)
	o := opts(135, 120, s)
	out := stripANSI(Render(s, o))
	checkFits(t, "portrait", out, o)
	ls := lines(out)
	if len(ls) != 120 || !strings.HasPrefix(ls[119], "q quit") {
		t.Fatalf("%d lines, last %q:\n%s", len(ls), ls[len(ls)-1], out)
	}
	for i := 1; i < len(ls); i++ {
		if strings.TrimSpace(ls[i]) == "" && strings.TrimSpace(ls[i-1]) == "" {
			t.Fatalf("blank band at line %d:\n%s", i, out)
		}
	}
	tp, g := titleLine(ls, "Throughput"), titleLine(ls, "GPU 0")
	if tp < 0 || g < 0 || tp == g {
		t.Fatalf("throughput %d gpu %d not stacked:\n%s", tp, g, out)
	}
	if n := chartBodies(ls)["generation tok/s"]; n < 4 || len(historyBody(ls)) != n {
		t.Fatalf("generation body %d rows, chart rows %d", len(historyBody(ls)), n)
	}

	o = opts(240, 67, s)
	out = stripANSI(Render(s, o))
	ls = lines(out)
	if tp, g := titleLine(ls, "Throughput"), titleLine(ls, "GPU 0"); tp < 0 || tp != g {
		t.Fatalf("landscape: throughput %d gpu %d not side by side:\n%s", tp, g, out)
	}
	if len(ls) != 67 || !strings.HasPrefix(ls[66], "q quit") || strings.TrimSpace(ls[65]) == "" {
		t.Fatalf("landscape not filled:\n%s", out)
	}

	// 135x24 is landscape here, so 7 rows are spare: one row of charts (generation
	// and power side by side, 5 rows each)
	o = opts(135, 24, s)
	ls = lines(stripANSI(Render(s, o)))
	if chartBodies(ls)["generation tok/s"] != 5 || titleLine(ls, "┌ power") != titleLine(ls, "┌ generation") ||
		titleLine(ls, "prompt tok/s") >= 0 {
		t.Fatalf("135x24:\n%s", strings.Join(ls, "\n"))
	}
	// fewer than 6 spare rows: no charts
	o = opts(135, 20, s)
	if out := Render(s, o); strings.Contains(out, "generation tok/s") {
		t.Fatalf("chart at 135x20:\n%s", stripANSI(out))
	}
}

// At 80 columns the header's right-hand items give way before the health status:
// the size first, then "range live"; a long range stays.
func TestHeaderGivesWay(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(80, 24, s)
	o.History = true
	h := lines(stripANSI(Render(s, o)))[0]
	if !strings.Contains(h, "│ ok") || !strings.Contains(h, "refresh 1s") || strings.Contains(h, "80x24") || strings.Contains(h, "range") {
		t.Errorf("80 live: %q", h)
	}
	o.Range = Range24h
	h = lines(stripANSI(Render(s, o)))[0]
	if !strings.Contains(h, "│ ok") || !strings.Contains(h, "range 24h") {
		t.Errorf("80 24h: %q", h)
	}
	o = opts(135, 24, s)
	o.History = true
	if h := lines(stripANSI(Render(s, o)))[0]; !strings.Contains(h, "range live │ refresh 1s │ 135x24") {
		t.Errorf("135: %q", h)
	}
}

func TestGridPlan(t *testing.T) {
	specs := make([]chartSpec, 7)
	for i := range specs {
		specs[i].key = fmt.Sprint(i)
	}
	shape := func(rows [][]chartSlot) string {
		var b strings.Builder
		for _, row := range rows {
			for _, c := range row {
				fmt.Fprintf(&b, "%s:%dx%d ", c.spec.key, c.w, c.h)
			}
			b.WriteString("| ")
		}
		return b.String()
	}
	for _, c := range []struct {
		spare, w, min int
		want          string
	}{
		{97, 135, 4, "0:67x23 1:68x23 | 2:67x22 3:68x22 | 4:67x22 5:68x22 | 6:135x22 | "},
		{24, 135, 4, "0:67x4 1:68x4 | 2:67x4 3:68x4 | 4:67x4 5:68x4 | 6:135x4 | "},
		{23, 135, 4, "0:67x6 1:68x6 | 2:67x6 3:68x6 | 4:67x5 5:68x5 | "},
		{6, 135, 4, "0:67x4 1:68x4 | "},
		{5, 135, 4, ""},
		{30, 100, 4, "0:100x4 | 1:100x4 | 2:100x4 | 3:100x4 | 4:100x4 | "},
		{11, 100, 4, "0:100x9 | "},
		// long ranges: 4 plot rows plus the time axis row
		{24, 135, 5, "0:67x6 1:68x6 | 2:67x6 3:68x6 | 4:67x6 5:68x6 | "},
		{7, 135, 5, "0:67x5 1:68x5 | "},
		{6, 135, 5, ""},
	} {
		if got := shape(gridPlan(specs, c.spare, c.w, c.min)); got != c.want {
			t.Errorf("gridPlan(%d, %d, %d) = %q, want %q", c.spare, c.w, c.min, got, c.want)
		}
	}
}

// chartTitles are the grid charts in priority order.
var chartTitles = []string{"generation tok/s", "power", "GPU load", "prompt tok/s", "GPU temperature", "VRAM %", "host CPU %"}

func TestChartGridTwoColumns(t *testing.T) {
	s := fullSnapshot(t)
	two := func(w int) bool {
		for _, l := range lines(stripANSI(Render(s, opts(w, 120, s)))) {
			n := 0
			for _, title := range chartTitles {
				if strings.Contains(l, "┌ "+title+" ") {
					n++
				}
			}
			if n >= 2 {
				return true
			}
		}
		return false
	}
	if !two(135) {
		t.Errorf("135x120: no line with two chart titles:\n%s", stripANSI(Render(s, opts(135, 120, s))))
	}
	if two(100) {
		t.Errorf("100x120: two charts side by side:\n%s", stripANSI(Render(s, opts(100, 120, s))))
	}
	out := stripANSI(Render(s, opts(135, 120, s)))
	for _, title := range chartTitles {
		if !strings.Contains(out, "┌ "+title+" ") {
			t.Errorf("135x120: no %q chart", title)
		}
	}
}

func TestChartPriorityDrop(t *testing.T) {
	s := fullSnapshot(t)
	for _, w := range []int{100, 135} {
		prev, fewest := len(chartTitles), len(chartTitles)
		for h := 120; h >= 10; h-- {
			out := stripANSI(Render(s, opts(w, h, s)))
			n := 0
			for n < len(chartTitles) && strings.Contains(out, "┌ "+chartTitles[n]+" ") {
				n++
			}
			for _, title := range chartTitles[n:] {
				if strings.Contains(out, "┌ "+title+" ") {
					t.Fatalf("%dx%d: %q shown after an earlier chart was dropped:\n%s", w, h, title, out)
				}
			}
			if landscape := w >= 100 && h*2 <= w; n > prev && prev > 0 && landscape == (w >= 100 && (h+1)*2 <= w) {
				// (Throughput and GPU side by side free rows: charts may come back; so
				// may a filler chart in rows left over once the grid is gone)
				t.Fatalf("%dx%d: %d charts, %d at a greater height", w, h, n, prev)
			}
			if n > 0 {
				fewest = min(fewest, n)
			}
			prev = n
		}
		// generation stands alone last: the grid's last chart at 100, the filler
		// chart (in rows left over once the grid is gone) at 135
		if fewest != 1 || prev != 0 {
			t.Fatalf("%d: fewest charts %d (want 1), at height 10: %d", w, fewest, prev)
		}
	}
}

// chartBodies returns the body height of each chart box in ls, by its title.
func chartBodies(ls []string) map[string]int {
	out := map[string]int{}
	rs := make([][]rune, len(ls))
	for i, l := range ls {
		rs[i] = []rune(l)
	}
	for i, l := range ls {
		for _, title := range chartTitles {
			j := strings.Index(l, "┌ "+title+" ")
			if j < 0 {
				continue
			}
			col := len([]rune(l[:j]))
			n := 0
			for k := i + 1; k < len(rs) && col < len(rs[k]) && rs[k][col] == '│'; k++ {
				n++
			}
			out[title] = n
		}
	}
	return out
}

func TestChartsFillHeight(t *testing.T) {
	s := fullSnapshot(t)
	for _, h := range []int{120, 90} {
		out := stripANSI(Render(s, opts(135, h, s)))
		ls := lines(out)
		if len(ls) != h || !strings.HasPrefix(ls[h-1], "q quit") {
			t.Fatalf("135x%d: %d lines, last %q", h, len(ls), ls[len(ls)-1])
		}
		for i := 1; i < len(ls); i++ {
			if strings.TrimSpace(ls[i]) == "" && strings.TrimSpace(ls[i-1]) == "" {
				t.Fatalf("135x%d: blank band at line %d:\n%s", h, i, out)
			}
		}
		if !strings.HasPrefix(ls[h-2], "└") {
			t.Fatalf("135x%d: the line above the footer is not a box bottom: %q", h, ls[h-2])
		}
		bodies := chartBodies(ls)
		lo, hi := 999, 0
		for _, n := range bodies {
			lo, hi = min(lo, n), max(hi, n)
		}
		if len(bodies) == 0 || lo < 4 || hi-lo > 1 {
			t.Fatalf("135x%d: chart bodies %v:\n%s", h, bodies, out)
		}
	}
}

func countCells(l string) int { return strings.Count(l, "█") + strings.Count(l, "░") }

func TestRenderScalesWithSize(t *testing.T) {
	s := busy(t, 300)
	var bar, chart [2]int
	for i, w := range []int{80, 118} { // one chart per row at both
		o := opts(w, 60, s)
		out := stripANSI(Render(s, o))
		checkFits(t, "scale", out, o)
		ls := lines(out)
		if !strings.Contains(ls[0], fmt.Sprintf("%dx60", w)) {
			t.Errorf("header lacks size: %q", ls[0])
		}
		bar[i] = countCells(ls[titleLine(ls, "generating")])
		for _, l := range historyBody(ls) {
			// chart cells: everything right of the y axis
			if j := strings.Index(l, "│"); j >= 0 {
				if k := strings.LastIndex(l, "│"); k > j {
					chart[i] = max(chart[i], lipgloss.Width(strings.TrimRight(l[j:k], " ")))
				}
			}
		}
	}
	if bar[1] <= bar[0] || chart[1] <= chart[0] {
		t.Fatalf("not wider at 118: bar %v chart %v", bar, chart)
	}
}

func TestRenderBusy(t *testing.T) {
	s := fixtureSnapshot(t)
	out := Render(s, opts(120, 30, s))
	for _, want := range []string{"example-model-27b", "generating", "Requests", "Throughput", "q quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, stripANSI(out))
		}
	}
}

func TestRenderASCII(t *testing.T) {
	s := busy(t, 120)
	for _, sz := range [][2]int{{135, 120}, {80, 24}, {240, 67}} {
		o := opts(sz[0], sz[1], s)
		o.ASCII = true
		out := stripANSI(Render(s, o))
		for _, r := range out {
			if r >= 128 {
				t.Fatalf("non-ASCII %q at %v:\n%s", r, sz, out)
			}
		}
		if !strings.Contains(out, "71C") {
			t.Fatalf("no 71C:\n%s", out)
		}
	}
}

func TestRenderLinuxConsole(t *testing.T) {
	s := busy(t, 120)
	o := opts(135, 120, s)
	o.Color16 = true
	out := Render(s, o)
	if strings.ContainsAny(out, "▁▂▃▄▅▆▇…") {
		t.Fatalf("glyph outside the console font:\n%s", stripANSI(out))
	}
	if !strings.Contains(out, "█") || !strings.Contains(out, "┌") {
		t.Fatalf("console mode should keep box drawing and blocks:\n%s", stripANSI(out))
	}
	// only the 16 basic colours: SGR 30-37, 90-97 (no 38;5 / 38;2)
	if strings.Contains(out, "38;5;") || strings.Contains(out, "38;2;") {
		t.Fatalf("non-16-colour SGR in console mode")
	}
}

func TestRenderNoColor(t *testing.T) {
	s := busy(t, 120)
	o := opts(135, 120, s)
	o.NoColor = true
	if out := Render(s, o); strings.Contains(out, "\x1b") {
		t.Fatalf("escape in NO_COLOR output")
	}
}

func TestRenderStale(t *testing.T) {
	s := idleSnapshot()
	o := opts(120, 30, s)
	o.Now = s.SlotsAt.Add(5 * time.Second)
	if out := stripANSI(Render(s, o)); !strings.Contains(out, "5s ago") {
		t.Fatalf("no age:\n%s", out)
	}
	o.Now = s.SlotsAt.Add(2 * time.Second)
	if out := stripANSI(Render(s, o)); strings.Contains(out, "ago") {
		t.Fatalf("fresh data marked stale:\n%s", out)
	}
}

func TestRenderUnreachable(t *testing.T) {
	s := unreachableSnapshot()
	out := stripANSI(Render(s, opts(120, 30, s)))
	if !strings.Contains(out, "unreachable since") || !strings.Contains(out, s.URL) {
		t.Fatalf("missing unreachable text:\n%s", out)
	}
}

func TestRenderHints(t *testing.T) {
	s := idleSnapshot()
	s.Slots, s.SlotsAt = nil, time.Time{}
	s.SlotsHint = "slots endpoint is disabled: start llama-server with --slots"
	s.MetricsHint = "start llama-server with --metrics for queue depth and totals"
	out := stripANSI(Render(s, opts(120, 30, s)))
	if !strings.Contains(out, s.SlotsHint) || !strings.Contains(out, s.MetricsHint) {
		t.Fatalf("hints missing:\n%s", out)
	}
	ls := lines(out)
	if i := titleLine(ls, s.MetricsHint); i < titleLine(ls, "Throughput") {
		t.Fatalf("metrics hint not in Throughput:\n%s", out)
	}
	if strings.Contains(out, "ctx used") {
		t.Fatalf("table shown with slots hint:\n%s", out)
	}
}

func TestRenderGPU(t *testing.T) {
	s := idleSnapshot()
	s.GPUs, s.GPUAt = oneGPU(t), s.At
	out := stripANSI(Render(s, opts(120, 40, s)))
	for _, want := range []string{"GPU 0 (this host)", "97%", "18.3/24.0 GB", "71°C", "285 W"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	o := opts(120, 40, s)
	o.ASCII = true
	if out := Render(s, o); !strings.Contains(out, "71C") {
		t.Errorf("ASCII temp:\n%s", stripANSI(out))
	}
	s.GPUs[0].HasPower = false
	if out := stripANSI(Render(s, opts(120, 40, s))); !strings.Contains(out, "n/a") {
		t.Errorf("no n/a:\n%s", out)
	}
	s.GPUs = nil
	if out := Render(s, opts(120, 40, s)); strings.Contains(out, "GPU") {
		t.Errorf("GPU panel without GPUs:\n%s", stripANSI(out))
	}
}

func TestRenderEnergy(t *testing.T) {
	s := idleSnapshot()
	s.HasPower, s.PowerW, s.EnergyWh, s.JoulesPerToken = true, 285, 420, 7.5
	s.PowerHist = hist(30)
	out := stripANSI(Render(s, opts(120, 40, s)))
	for _, want := range []string{"Energy", "285 W", "420 Wh", "7.5 J/token", "480 tok/Wh"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cpu") {
		t.Errorf("split line without CPU power:\n%s", out)
	}
	s.GPUs, s.HasGPUPower, s.GPUPowerW, s.HasCPUPower, s.CPUPowerW, s.PowerW = oneGPU(t), true, 285, true, 127, 412
	out = stripANSI(Render(s, opts(120, 40, s)))
	if !strings.Contains(out, "gpu 285 W   cpu 127 W") || !strings.Contains(out, "412 W") {
		t.Errorf("split line:\n%s", out)
	}
	// GPU power expired from the totals (query failing): the GPUs still report
	// power, but the split is not shown (HasGPUPower decides, not the GPU list)
	gs, hs, gw, pw := s.GPUs, s.HasGPUPower, s.GPUPowerW, s.PowerW
	s.HasGPUPower, s.GPUPowerW, s.PowerW = false, 0, 127
	if out := stripANSI(Render(s, opts(120, 40, s))); strings.Contains(out, "gpu 0 W") || strings.Contains(out, "cpu 127 W") {
		t.Errorf("split line without GPU power in the totals:\n%s", out)
	}
	s.GPUs, s.HasGPUPower, s.GPUPowerW, s.PowerW = gs, hs, gw, pw
	s.EnergyWh, s.JoulesPerToken = 1234, 0
	out = stripANSI(Render(s, opts(120, 40, s)))
	if !strings.Contains(out, "1.23 kWh") || strings.Contains(out, "J/token") {
		t.Errorf("kWh / idle J/token rule:\n%s", out)
	}
	s.HasPower = false
	if out := Render(s, opts(120, 40, s)); strings.Contains(out, "Energy") {
		t.Errorf("Energy without power:\n%s", stripANSI(out))
	}
}

func TestRenderManySlots(t *testing.T) {
	s := idleSnapshot()
	s.Slots = nil
	for i := 0; i < 64; i++ {
		s.Slots = append(s.Slots, stats.SlotView{Slot: llama.Slot{ID: i, NCtx: 4096}})
	}
	o := opts(120, 20, s)
	out := Render(s, o)
	checkFits(t, "many", out, o)
	if !strings.Contains(out, "more") {
		t.Fatalf("no 'more':\n%s", stripANSI(out))
	}
}

func TestRenderNoSecrets(t *testing.T) {
	props := `{"total_slots":1,"model_alias":"m","build_info":"b1","endpoint_slots":true,"endpoint_metrics":false,
	 "chat_template":"TEMPLATE_SENTINEL","default_generation_settings":{"n_ctx":4096,"params":{"generation_prompt":"PROMPT_SENTINEL"}},
	 "media_marker":"TEMPLATE_SENTINEL"}`
	slots := `[{"id":0,"n_ctx":4096,"is_processing":true,"id_task":1,"n_prompt_tokens":100,"n_prompt_tokens_processed":10,"n_prompt_tokens_cache":90,
	 "params":{"generation_prompt":"PROMPT_SENTINEL"},"next_token":[{"n_decoded":5,"n_remain":-1}]}]`
	srv := fakeserver.New([]fakeserver.Step{{
		Health:  fakeserver.Response{Status: 200, Body: []byte(`{"status":"ok"}`)},
		Props:   fakeserver.Response{Status: 200, Body: []byte(props)},
		Slots:   fakeserver.Response{Status: 200, Body: []byte(slots)},
		Metrics: fakeserver.Response{Status: 501},
	}})
	defer srv.Close()
	c := collect.New(collect.Config{URL: srv.URL, Interval: time.Second, GPU: collect.GPUOff})
	s := c.Poll(context.Background(), t0)
	out := Render(s, opts(135, 60, s))
	if !strings.Contains(out, "prompt eval") && !strings.Contains(out, "generating") {
		t.Fatalf("unexpected output:\n%s", stripANSI(out))
	}
	if strings.Contains(out, "SENTINEL") {
		t.Fatalf("leaked secret:\n%s", out)
	}
}

func TestDetectOptions(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if o := DetectOptions(env(map[string]string{"TERM": "linux"})); !o.Color16 || o.NoColor {
		t.Errorf("TERM=linux: %+v", o)
	}
	if o := DetectOptions(env(map[string]string{"TERM": "xterm-256color", "NO_COLOR": "1"})); o.Color16 || !o.NoColor {
		t.Errorf("NO_COLOR: %+v", o)
	}
}

// realistic returns 120 samples of two requests with idle gaps: generation and
// prompt tok/s and total power in watts.
func realistic() (gen, prompt, power []float64) {
	for i := 0; i < 120; i++ {
		g, p, w := 0.0, 0.0, 95.0
		switch {
		case i >= 8 && i < 10, i >= 66 && i < 68: // prompt eval
			p, w = 1400-float64(i%2)*300, 455
		case i >= 10 && i < 52:
			g, w = 88-float64(i-10)*0.4, 400-float64(i%3)*6
		case i >= 68 && i < 120:
			g, w = 84-float64(i-68)*0.3, 395-float64(i%4)*5
		}
		gen, prompt, power = append(gen, g), append(prompt, p), append(power, w)
	}
	return gen, prompt, power
}

// TestSyntheticFrame writes the console-mode portrait frame of a synthetic busy
// snapshot to $LLAMADASH_FRAME_OUT when set (used to review the layout).
func TestSyntheticFrame(t *testing.T) {
	s := busy(t, 120)
	s.GenHist, s.PromptHist, s.PowerHist = realistic()
	s.GenRate, s.PromptRate = s.GenHist[119], 0
	s.Slots[0].GenRate = s.GenRate
	s.MetricsHint = ""
	s.Metrics = llama.Metrics{"requests_deferred": 2, "requests_processing": 1,
		"tokens_predicted_total": 1.2e6, "prompt_tokens_total": 8.4e6}
	s.JoulesPerToken = s.PowerW / s.GenRate
	o := opts(135, 120, s)
	o.Color16 = true
	out := Render(s, o)
	checkFits(t, "synthetic", out, o)
	if p := os.Getenv("LLAMADASH_FRAME_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(stripANSI(out)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var sgrFg = regexp.MustCompile(`\x1b\[[0-9;]*?38;5;([0-9]+)[0-9;]*m`)

var emptyRun = regexp.MustCompile(`\x1b\[[0-9;]*m\x1b\[m`)

// panelContent returns the raw (styled) content lines of the panel titled title,
// cut to the panel's columns (charts of the grid share their lines).
func panelContent(raw []string, title string) []string {
	i, left, right := -1, 0, 0
	for j, l := range raw {
		plain := stripANSI(l)
		if k := strings.Index(plain, "┌ "+title); k >= 0 {
			i, left = j, len([]rune(plain[:k]))
			right = left + len([]rune(plain[k:]))
			if e := strings.Index(plain[k:], "┐"); e >= 0 {
				right = left + len([]rune(plain[k:k+e])) + 1
			}
			break
		}
	}
	if i < 0 {
		return nil
	}
	var out []string
	for _, l := range raw[i+1:] {
		c := emptyRun.ReplaceAllString(ansi.Cut(l, left, right), "") // Cut keeps the styles of what it cut off
		if strings.HasPrefix(stripANSI(c), "└") {
			break
		}
		out = append(out, c)
	}
	return out
}

// panelHead returns the title line of the panel titled title, cut to its columns.
func panelHead(plain []string, title string) string {
	for _, l := range plain {
		if k := strings.Index(l, "┌ "+title); k >= 0 {
			head := l[k:]
			if e := strings.Index(head, "┐"); e >= 0 {
				head = head[:e+len("┐")]
			}
			return head
		}
	}
	return ""
}

// dimmed reports whether every coloured run in lines is the dim or border colour.
func dimmed(lines []string) bool {
	for _, l := range lines {
		for _, m := range sgrFg.FindAllStringSubmatch(l, -1) {
			if m[1] != "243" && m[1] != "66" { // palette256 rDim, rBorder
				return false
			}
		}
	}
	return true
}

func TestRenderStaleBySource(t *testing.T) {
	base := busy(t, 120)
	base.GenHist, base.PromptHist, base.PowerHist = realistic()
	base.GPUUtilHist, base.GPUMemHist, base.GPUTempHist = gpuHists()
	base.PromptHist[115] = 900 // a prompt in the newest samples, so the prompt chart has bars to colour
	server := []string{"Requests", "Throughput", "generation tok/s", "prompt tok/s"}
	gpuSrc := []string{"GPU 0", "Energy", "power", "GPU load", "GPU temperature", "VRAM %"}
	check := func(name string, s collect.Snapshot, stale, fresh []string) {
		t.Helper()
		o := opts(135, 120, s)
		out := Render(s, o)
		raw, plain := lines(out), lines(stripANSI(out))
		for _, p := range stale {
			c := panelContent(raw, p)
			title := panelHead(plain, p)
			if len(c) == 0 || !dimmed(c) || !strings.Contains(title, "stale, ") || !strings.Contains(title, "s ago") {
				t.Errorf("%s: %s not dimmed/tagged: %q", name, p, title)
			}
		}
		for _, p := range fresh {
			c := panelContent(raw, p)
			title := panelHead(plain, p)
			if len(c) == 0 || dimmed(c) || strings.Contains(title, "stale") {
				t.Errorf("%s: fresh %s dimmed or tagged: %q", name, p, title)
			}
		}
	}
	check("fresh", base, nil, append(server, gpuSrc...))

	s := base
	s.SlotsAt = s.At.Add(-5 * time.Second)
	check("server stale", s, server, gpuSrc)

	s = base
	s.GPUAt = s.At.Add(-7 * time.Second) // GPU threshold is 3 x 2s
	check("gpu stale", s, gpuSrc, server)

	// CPU-only power has no timestamp of its own: the Energy panel stays fresh
	s = base
	s.GPUs, s.GPUAt, s.GPUPowerW = nil, s.At.Add(-time.Minute), 0
	check("cpu only", s, nil, []string{"Energy", "power"})

	// NoColor: the tag alone carries it
	s = base
	s.SlotsAt, s.GPUAt = s.At.Add(-5*time.Second), s.At.Add(-7*time.Second)
	o := opts(135, 120, s)
	o.NoColor = true
	out := Render(s, o)
	if strings.Count(out, "stale, ") != 10 || strings.Contains(out, "\x1b") {
		t.Errorf("NoColor stale tags: %d", strings.Count(out, "stale, "))
	}
}

// /health answering an error status or a non-JSON body must not show "ok".
func TestRenderHealthError(t *testing.T) {
	for name, health := range map[string]fakeserver.Response{
		"500":      {Status: 500, Body: []byte(`{"error":"boom"}`)},
		"not json": {Status: 200, Body: []byte(`<html>proxy</html>`)},
	} {
		srv := fakeserver.New([]fakeserver.Step{{
			Health:  health,
			Props:   fakeserver.Response{Status: 200, Body: []byte(`{"total_slots":1,"model_alias":"m","endpoint_metrics":false}`)},
			Slots:   fakeserver.Response{Status: 200, Body: []byte(`[]`)},
			Metrics: fakeserver.Response{Status: 501},
		}})
		c := collect.New(collect.Config{URL: srv.URL, Interval: time.Second, GPU: collect.GPUOff})
		s := c.Poll(context.Background(), t0)
		srv.Close()
		o := opts(120, 30, s)
		o.NoColor = true
		header := lines(Render(s, o))[0]
		if !s.Reachable || strings.Contains(header, " ok") || !strings.Contains(header, "error") {
			t.Errorf("%s: header %q", name, header)
		}
		if plain := RenderPlain(s); !strings.Contains(plain, "health error") {
			t.Errorf("%s: plain:\n%s", name, plain)
		}
	}
}

func manySlots(n int, queued bool) collect.Snapshot {
	s := idleSnapshot()
	s.Slots = nil
	for i := 0; i < n; i++ {
		s.Slots = append(s.Slots, stats.SlotView{Slot: llama.Slot{ID: i, NCtx: 4096}})
	}
	if queued {
		s.Metrics = llama.Metrics{"requests_deferred": 2, "requests_processing": 1}
	}
	return s
}

// A limit of 0 lines with the queued line must not turn into "no limit".
func TestRequestsZeroBudget(t *testing.T) {
	s := manySlots(5, true)
	r := newRenderer(s, opts(120, 30, s))
	if got := r.requestsLines(116, 0); len(got) > 2 {
		t.Fatalf("maxInner 0: %d lines (every slot rendered)", len(got))
	}
}

// requestsHeight counts the Requests lines without rendering them.
func TestRequestsHeight(t *testing.T) {
	hint := idleSnapshot()
	hint.Slots, hint.SlotsHint = nil, strings.Repeat("unrecognised /slots response ", 9)
	snaps := map[string]collect.Snapshot{
		"many": manySlots(12, false), "many queued": manySlots(12, true), "none": manySlots(0, false),
		"none queued": manySlots(0, true), "hint": hint, "unreachable": unreachableSnapshot(), "empty": {},
	}
	for name, s := range snaps {
		for _, iw := range []int{20, 76, 131} {
			r := newRenderer(s, opts(iw+4, 30, s))
			if got, want := r.requestsHeight(iw), len(r.requestsLines(iw, -1)); got != want {
				t.Errorf("%s at %d: height %d, lines %d", name, iw, got, want)
			}
		}
	}
}

// bare is the realistic snapshot without Recent requests and without any local
// sensor: no GPU, host, process, container or power.
func bare(t *testing.T) collect.Snapshot {
	s := fullSnapshot(t)
	s.Recent = nil
	s.GPUs, s.Host, s.Proc, s.Container = nil, nil, nil, nil
	s.HasPower, s.HasGPUPower, s.HasCPUPower = false, false, false
	s.PowerHist, s.HostCPUHist, s.GPUUtilHist, s.GPUMemHist, s.GPUTempHist = nil, nil, nil, nil, nil
	s.Cost = collect.Cost{}
	return s
}

// Leftover rows go to the charts, then Recent requests, then a filler chart: from
// a height of 23 no frame has a run of more than one blank line.
func TestNoBlankBands(t *testing.T) {
	snaps := map[string]collect.Snapshot{"full": fullSnapshot(t), "bare": bare(t)}
	for name, s := range snaps {
		for _, rng := range []Range{Range5m, Range24h} {
			for _, w := range []int{80, 100, 119, 120, 135} {
				for h := 10; h <= 130; h++ {
					o := opts(w, h, s)
					o.History, o.Range = true, rng
					if rng.long() {
						o.LongAt, o.Long = s.At, map[string][]float64{}
						for key, n := range chartPlotWidths(s, o) {
							vals := make([]float64, n)
							for i := range vals {
								vals[i] = 20 + float64(i%7)*5
								if i%11 == 3 {
									vals[i] = math.NaN()
								}
							}
							o.Long[key] = vals
						}
					}
					ls := lines(stripANSI(Render(s, o)))
					if len(ls) != h {
						t.Fatalf("%s %v %dx%d: %d lines", name, rng, w, h, len(ls))
					}
					for i, l := range ls {
						if n := len([]rune(l)); n != w {
							t.Fatalf("%s %v %dx%d: line %d is %d wide", name, rng, w, h, i, n)
						}
					}
					if !strings.HasPrefix(ls[h-1], "q quit") {
						t.Fatalf("%s %v %dx%d: footer not last: %q", name, rng, w, h, ls[h-1])
					}
					if h < 23 {
						continue
					}
					for i := 1; i < len(ls); i++ {
						if strings.TrimSpace(ls[i]) == "" && strings.TrimSpace(ls[i-1]) == "" {
							t.Fatalf("%s %v %dx%d: blank band at line %d:\n%s", name, rng, w, h, i, strings.Join(ls, "\n"))
						}
					}
				}
			}
		}
	}
}

// The live title states the time the ring really covers, from the collector's
// timestamps: after a change from 1 s to 10 s polls the ring's length times the
// interval would be wrong.
func TestLiveSpanFromTimestamps(t *testing.T) {
	s := fullSnapshot(t)
	s.GenHist = []float64{40, 41, 42, 43, 44, 45, 46} // 5 at 1 s, then 2 at 10 s
	s.GenHistFrom = s.At.Add(-24 * time.Second)
	o := opts(135, 120, s)
	o.Interval = 10 * time.Second
	if title := panelTitle(stripANSI(Render(s, o)), "generation tok/s"); !strings.Contains(title, "last 24s ") {
		t.Errorf("title %q, want last 24s", title)
	}
}
