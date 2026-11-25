package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"llamadash/internal/collect"
	"llamadash/internal/fakeserver"
)

func newTestModel(t *testing.T) tea.Model { return newSourceModel(t, nil) }

// newSourceModel is a model with history source src (nil: none) whose ticks and
// history refresh timers fire at once.
func newSourceModel(t *testing.T, src SeriesSource) tea.Model {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	t.Cleanup(srv.Close)
	c := collect.New(collect.Config{URL: srv.URL, Interval: time.Second, GPU: collect.GPUOff})
	m := NewModel(c, time.Second, Options{}, src)
	if src != nil {
		m.(*Model).after = func(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
			return func() tea.Msg { return f(time.Time{}) }
		}
	}
	return m
}

func TestRangeCycle(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}}
	m := longModel(t, src, 135, 120)
	if v := m.View().Content; !strings.Contains(v, "range live") || !strings.Contains(lastLine(v), "h range") {
		t.Fatalf("5m: header/footer:\n%s", stripANSI(v))
	}
	for i, want := range []Range{Range1h, Range24h, Range7d, Range5m} {
		var cmd tea.Cmd
		m, cmd = update(m, key("h"))
		if got := m.(*Model).opts.Range; got != want {
			t.Fatalf("press %d: range %v want %v", i+1, got, want)
		}
		if l := findLong(cmd); l != nil {
			m, _ = update(m, *l)
		}
		if want == Range24h {
			if v := stripANSI(m.View().Content); !strings.Contains(lines(v)[0], "range 24h") {
				t.Fatalf("header: %q", lines(v)[0])
			}
		}
	}
	// without a source "h" does nothing and the footer does not offer it
	m = settle(t, newTestModel(t))
	m, cmd := update(m, key("h"))
	if cmd != nil || m.(*Model).opts.Range != Range5m {
		t.Fatalf("h without a source: range %v cmd %v", m.(*Model).opts.Range, cmd != nil)
	}
	if v := m.View().Content; strings.Contains(lastLine(v), "h range") || strings.Contains(v, "range ") {
		t.Fatalf("footer or header offers ranges without a source:\n%s", stripANSI(v))
	}
}

func lastLine(s string) string {
	ls := lines(stripANSI(s))
	if len(ls) == 0 {
		return ""
	}
	return ls[len(ls)-1]
}

func TestLongRangeFetchOffUIThread(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}, block: make(chan struct{})}
	m := longModel(t, src, 135, 120)
	start := time.Now()
	m, cmd := update(m, key("h"))
	v := stripANSI(m.View().Content)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Update and View took %v", d)
	}
	if len(src.seen()) != 0 || cmd == nil {
		t.Fatalf("Series called in Update/View (%d calls) or no fetch command", len(src.seen()))
	}
	if c := panelTitle(v, "generation tok/s"); !strings.Contains(c, "1h") || !strings.Contains(c, "loading") {
		t.Fatalf("generation chart title while loading: %q", c)
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }() // blocks in Series
	for len(src.seen()) == 0 {
		time.Sleep(time.Millisecond)
	}
	// while it is in flight nothing starts another fetch: a refresh timer, a resize,
	// a poll result
	m, cmd2 := update(m, refetchMsg{gen: m.(*Model).longGen})
	m, cmd3 := update(m, tea.WindowSizeMsg{Width: 100, Height: 60})
	m, _ = update(m, pollMsg{snap: fullSnapshot(t)})
	if cmd2 != nil || cmd3 != nil || len(src.seen()) != 1 {
		t.Fatalf("second fetch while one is in flight: timer %v resize %v calls %d", cmd2 != nil, cmd3 != nil, len(src.seen()))
	}
	close(src.block)
	m, next := update(m, <-got)
	// the result is for the old plot widths: still loading, and the refetch for the
	// new ones starts at once
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "loading") {
		t.Fatalf("data for the old widths drawn: %q", title)
	}
	l := findLong(next)
	if l == nil {
		t.Fatal("the resize during the fetch did not refetch afterwards")
	}
	m, _ = update(m, *l)
	v = stripANSI(m.View().Content)
	c := panelContent(lines(v), "generation tok/s")
	if title := panelTitle(v, "generation tok/s"); strings.Contains(title, "loading") || len(c) == 0 ||
		!strings.Contains(c[len(c)-2], "█") || !strings.Contains(c[len(c)-1], "now") || !strings.Contains(c[0], " 88 tok/s") {
		t.Fatalf("result did not fill the chart: %q\n%s", title, strings.Join(c, "\n"))
	}
}

// panelTitle returns the title line of the panel titled title (or "").
func panelTitle(out, title string) string {
	ls := lines(out)
	if i := titleLine(ls, "┌ "+title); i >= 0 {
		return ls[i]
	}
	return ""
}

func TestSeriesErrorShown(t *testing.T) {
	src := &fakeSource{err: errBusy}
	m := longModel(t, src, 135, 120)
	m = pressAndFetch(t, m, "h")
	v := stripANSI(m.View().Content)
	for _, title := range []string{"generation tok/s", "power", "prompt tok/s"} {
		if !strings.Contains(panelTitle(v, title), "history unavailable") {
			t.Errorf("%s title: %q", title, panelTitle(v, title))
		}
	}
	for _, want := range []string{"generating", "┌ Requests", "┌ Throughput", "┌ Energy", "64°C"} {
		if !strings.Contains(v, want) {
			t.Errorf("live panels affected: no %q\n%s", want, v)
		}
	}
}

func TestRefetchOnResize(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}}
	m := longModel(t, src, 135, 120)
	m = pressAndFetch(t, m, "h")
	first := src.seen()
	// at 135 the generation chart is the left half of the grid: 67 wide, 63 inside
	if b := bucketsFor(first, "gen_rate"); b != 63-longLabelW-1 {
		t.Fatalf("buckets at 135: %d", b)
	}
	m, cmd := update(m, tea.WindowSizeMsg{Width: 100, Height: 60})
	l := findLong(cmd)
	if l == nil {
		t.Fatal("resize did not refetch")
	}
	m, _ = update(m, *l)
	calls := src.seen()[len(first):]
	if b := bucketsFor(calls, "gen_rate"); b != 96-longLabelW-1 {
		t.Fatalf("buckets at 100: %d", b)
	}
	for _, c := range calls {
		if d := c.to.Sub(c.from); d != time.Hour {
			t.Fatalf("range %v", d)
		}
	}
	// the same size again: no fetch
	if _, cmd := update(m, tea.WindowSizeMsg{Width: 100, Height: 60}); findLong(cmd) != nil {
		t.Fatal("refetch without a change")
	}
}

func bucketsFor(calls []seriesCall, metric string) int {
	for _, c := range calls {
		if c.metric == metric {
			return c.buckets
		}
	}
	return -1
}

func key(s string) tea.KeyPressMsg {
	if s == "ctrl+c" {
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func update(m tea.Model, msg tea.Msg) (tea.Model, tea.Cmd) { return m.Update(msg) }

func TestKeysQuit(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		_, cmd := update(newTestModel(t), key(k))
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: not tea.Quit", k)
		}
	}
}

// settle delivers a poll result so the model is idle and waiting for its tick.
func settle(t *testing.T, m tea.Model) tea.Model {
	m, cmd := update(m, pollMsg{snap: collect.Snapshot{At: t0}})
	if cmd == nil {
		t.Fatal("poll result did not schedule a tick")
	}
	return m
}

func tickOf(m tea.Model) tickMsg { return tickMsg{gen: m.(*Model).tickGen} }

// Pause freezes the display only: polling (and with it recording) goes on, and
// resuming shows the latest snapshot at once.
func TestPauseFreezesDisplayKeepsPolling(t *testing.T) {
	m := newTestModel(t)
	m = settle(t, m)
	m, _ = update(m, key("p"))
	if v := m.View().Content; !strings.Contains(v, "paused") || strings.Contains(v, "still recording") {
		t.Fatal("header does not show paused (no database: nothing about recording)")
	}
	m, cmd := update(m, tickOf(m))
	if cmd == nil {
		t.Fatal("tick while paused issued no poll")
	}
	later := collect.Snapshot{At: t0.Add(5 * time.Second), URL: "http://later:1"}
	gen := m.(*Model).tickGen
	m, cmd = update(m, pollMsg{snap: later})
	if cmd == nil || m.(*Model).tickGen != gen+1 {
		t.Fatal("poll result while paused scheduled no next tick")
	}
	if v := stripANSI(m.View().Content); strings.Contains(v, "http://later:1") || !m.(*Model).snap.At.Equal(t0) {
		t.Fatalf("the display changed while paused:\n%s", v)
	}
	if _, cmd := update(m, tickOf(m)); cmd == nil {
		t.Fatal("the next tick while paused issued no poll")
	}
	m, _ = update(m, key("p"))
	if v := stripANSI(m.View().Content); strings.Contains(v, "paused") || !strings.Contains(v, "http://later:1") {
		t.Fatalf("resume did not show the latest snapshot:\n%s", v)
	}
}

// While paused the frozen picture is drawn against the time it was frozen: polls
// go on, but nothing turns stale and the container's uptime does not tick.
func TestPauseFreezesClock(t *testing.T) {
	m := newTestModel(t)
	clock := fullAt
	m.(*Model).now = func() time.Time { return clock }
	m, _ = update(m, tea.WindowSizeMsg{Width: 135, Height: 120})
	m, _ = update(m, pollMsg{snap: fullSnapshot(t)})
	if v := stripANSI(m.View().Content); strings.Contains(v, "stale") || !strings.Contains(v, "up 3h12m") {
		t.Fatalf("live view:\n%s", v)
	}
	m, _ = update(m, key("p"))
	clock = fullAt.Add(time.Minute) // SlotsAt is now 60 intervals old
	later := fullSnapshot(t)
	later.At = clock
	m, _ = update(m, pollMsg{snap: later})
	if v := stripANSI(m.View().Content); strings.Contains(v, "stale") || !strings.Contains(v, "up 3h12m") {
		t.Fatalf("paused view moved with the clock:\n%s", v)
	}
	m, _ = update(m, key("p")) // live again: the real clock
	if v := stripANSI(m.View().Content); !strings.Contains(v, "stale, 1m00s ago") || !strings.Contains(v, "up 3h13m") {
		t.Fatalf("resumed view:\n%s", v)
	}
}

// A long-range result that arrives while paused is held and drawn on resume; h
// does nothing while paused (the header says paused).
func TestPauseHoldsLongRange(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}}
	m := longModel(t, src, 135, 120)
	m = pressAndFetch(t, m, "h")
	// the history raises the chart top above the live data's peak of 88 tok/s:
	// the top label tells which result is drawn
	top := func() string { return panelContent(lines(stripANSI(m.View().Content)), "generation tok/s")[0] }
	if !strings.Contains(top(), " 88 tok/s") {
		t.Fatalf("before: %q", top())
	}
	m, _ = update(m, key("p"))
	src.mu.Lock()
	src.vals["gen_rate"] = 280
	src.mu.Unlock()
	m, cmd := update(m, refetchMsg{gen: m.(*Model).longGen})
	l := findLong(cmd)
	if l == nil {
		t.Fatal("no refetch while paused")
	}
	m, _ = update(m, *l)
	if !strings.Contains(top(), " 88 tok/s") {
		t.Fatalf("a result applied while paused: %q", top())
	}
	if m2, cmd := update(m, key("h")); cmd != nil || m2.(*Model).opts.Range != Range1h {
		t.Fatalf("h while paused: range %v, cmd %v", m2.(*Model).opts.Range, cmd != nil)
	}
	m, _ = update(m, key("p"))
	if !strings.Contains(top(), "280 tok/s") {
		t.Fatalf("the held result was not drawn on resume: %q", top())
	}
}

// With a database the paused header says recording goes on.
func TestPausedStillRecording(t *testing.T) {
	m := longModel(t, &fakeSource{vals: map[string]float64{"gen_rate": 40}}, 135, 120)
	m, _ = update(m, key("p"))
	if h := lines(stripANSI(m.View().Content))[0]; !strings.Contains(h, "paused (still recording)") {
		t.Fatalf("header: %q", h)
	}
	ro := fullSnapshot(t)
	ro.DBReadOnly = true // another dashboard records, this one does not
	m, _ = update(m, pollMsg{snap: ro})
	m, _ = update(m, key("p"))
	m, _ = update(m, key("p"))
	if h := lines(stripANSI(m.View().Content))[0]; !strings.Contains(h, "paused") || strings.Contains(h, "still recording") {
		t.Fatalf("read-only header: %q", h)
	}
}

func TestIntervalSteps(t *testing.T) {
	m := newTestModel(t)
	interval := func() time.Duration { return m.(*Model).interval }
	for _, want := range []time.Duration{500 * time.Millisecond, 250 * time.Millisecond, 250 * time.Millisecond} {
		m, _ = update(m, key("+"))
		if interval() != want {
			t.Fatalf("+: %v want %v", interval(), want)
		}
	}
	m = newTestModel(t)
	for _, want := range []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second} {
		m, _ = update(m, key("-"))
		if interval() != want {
			t.Fatalf("-: %v want %v", interval(), want)
		}
	}
	if !strings.Contains(m.View().Content, "refresh 10s") {
		t.Fatal("header does not show the interval")
	}
}

func TestNoOverlappingPolls(t *testing.T) {
	m := newTestModel(t)
	m = settle(t, m)
	tick := tickOf(m)
	m, first := update(m, tick)
	if first == nil {
		t.Fatal("tick issued no poll")
	}
	if _, second := update(m, tick); second != nil {
		t.Fatal("second tick before the poll result issued another poll")
	}
	// a tick from an older chain is ignored as well
	m = settle(t, m)
	if _, cmd := update(m, tickMsg{gen: tick.gen - 1}); cmd != nil {
		t.Fatal("stale tick issued a poll")
	}
}

func TestResize(t *testing.T) {
	m := newTestModel(t)
	m = settle(t, m)
	m, _ = update(m, tea.WindowSizeMsg{Width: 40, Height: 8})
	v := m.View()
	ls := strings.Split(v.Content, "\n")
	if len(ls) > 8 {
		t.Fatalf("%d lines", len(ls))
	}
	for _, l := range ls {
		if lipgloss.Width(l) > 40 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	if !v.AltScreen {
		t.Fatal("not on the alternate screen")
	}
}

func TestDefaultSize(t *testing.T) {
	m := settle(t, newTestModel(t))
	if ls := strings.Split(m.View().Content, "\n"); len(ls) != 24 || !strings.Contains(ls[0], "80x24") {
		t.Fatalf("default size: %d lines, header %q", len(ls), ls[0])
	}
}

// The interval reaches the collector inside the poll command, just before Poll,
// so rapid key presses cannot be applied out of order.
func TestIntervalReachesCollector(t *testing.T) {
	m := newTestModel(t)
	var got []time.Duration
	m.(*Model).setInterval = func(d time.Duration) { got = append(got, d) }
	first := m.Init() // a poll is in flight, so the key presses schedule nothing
	for _, k := range []string{"+", "-", "-"} {
		var cmd tea.Cmd
		if m, cmd = update(m, key(k)); cmd != nil {
			t.Fatalf("%s: command while a poll is in flight", k)
		}
	}
	if len(got) != 0 {
		t.Fatalf("collector changed outside a poll: %v", got)
	}
	m, _ = update(m, first())  // the first poll applies the interval it was started with
	m, _ = update(m, key("-")) // idle: the new interval waits for the next poll
	_, cmd := update(m, tickOf(m))
	if cmd == nil {
		t.Fatal("tick issued no poll")
	}
	m, _ = update(m, cmd())
	want := []time.Duration{time.Second, 5 * time.Second}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("collector intervals %v want %v", got, want)
	}
	if m.(*Model).interval != 5*time.Second {
		t.Fatalf("model interval %v", m.(*Model).interval)
	}
}

func TestNoWindowTitle(t *testing.T) {
	if v := settle(t, newTestModel(t)).View(); v.WindowTitle != "" {
		t.Fatalf("window title %q: an OSC title sequence on the bare console", v.WindowTitle)
	}
}

func TestIntervalChangeReschedulesTick(t *testing.T) {
	m := settle(t, newTestModel(t)) // idle, a 1s tick pending
	old := tickOf(m)
	m, cmd := update(m, key("-"))
	if cmd == nil || m.(*Model).tickGen != old.gen+1 {
		t.Fatalf("interval change did not schedule a new tick (gen %d -> %d)", old.gen, m.(*Model).tickGen)
	}
	if _, cmd := update(m, old); cmd != nil {
		t.Fatal("the tick with the old interval still polls")
	}
	if _, cmd := update(m, tickOf(m)); cmd == nil {
		t.Fatal("the new tick does not poll")
	}

	// paused: polling goes on, so the tick follows the new interval as well
	m = settle(t, newTestModel(t))
	m, _ = update(m, key("p"))
	gen := m.(*Model).tickGen
	if m, _ = update(m, key("+")); m.(*Model).tickGen != gen+1 {
		t.Fatal("no new tick while paused")
	}
	// polling: no new tick
	m = newTestModel(t)
	m.Init()
	gen = m.(*Model).tickGen
	if m, _ = update(m, key("+")); m.(*Model).tickGen != gen {
		t.Fatal("tick scheduled while a poll is in flight")
	}
}

// Data fetched for one plot width is not drawn under another: after a resize the
// charts say "loading" until the result for the new widths arrives.
func TestStaleWidthsShowLoading(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}}
	m := longModel(t, src, 135, 120)
	m = pressAndFetch(t, m, "h")
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "avg per column") {
		t.Fatalf("before the resize: %q", title)
	}
	m, cmd := update(m, tea.WindowSizeMsg{Width: 100, Height: 60})
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "loading") {
		t.Fatalf("after the resize, before the refetch: %q", title)
	}
	l := findLong(cmd)
	if l == nil {
		t.Fatal("no refetch")
	}
	m, _ = update(m, *l)
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "avg per column") {
		t.Fatalf("after the refetch: %q", title)
	}
}

// A Series call that hangs: the fetch gives up after its timeout and the charts
// say "history unavailable"; no other Series call starts until the hung one has
// returned.
func TestFetchTimeout(t *testing.T) {
	src := &fakeSource{vals: map[string]float64{"gen_rate": 40}, block: make(chan struct{})}
	m := longModel(t, src, 135, 120)
	m.(*Model).fetchTimeout = 20 * time.Millisecond
	m, cmd := update(m, key("h"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the fetch did not give up")
	}
	m, wait := update(m, msg)
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "history unavailable") {
		t.Fatalf("after the timeout: %q", title)
	}
	if wait == nil {
		t.Fatal("no command waiting for the hung call")
	}
	// nothing starts another Series call while the first one hangs
	m, c1 := update(m, refetchMsg{gen: m.(*Model).longGen})
	m, c2 := update(m, tea.WindowSizeMsg{Width: 100, Height: 60})
	m, c3 := update(m, key("h")) // 24h
	if c1 != nil || c2 != nil || c3 != nil || len(src.seen()) != 1 {
		t.Fatalf("fetch while a call hangs: %v %v %v, %d calls", c1 != nil, c2 != nil, c3 != nil, len(src.seen()))
	}
	go func() { done <- wait() }()
	select {
	case <-done:
		t.Fatal("the wait returned while the call still hangs")
	case <-time.After(20 * time.Millisecond):
	}
	close(src.block)
	m, next := update(m, <-done)
	l := findLong(next)
	if l == nil {
		t.Fatal("no fetch once the hung call returned")
	}
	m, _ = update(m, *l)
	if title := panelTitle(stripANSI(m.View().Content), "generation tok/s"); !strings.Contains(title, "24h, avg per column") {
		t.Fatalf("after the hung call returned: %q", title)
	}
}
