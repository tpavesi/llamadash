package ui

import (
	"context"
	"errors"
	"maps"
	"time"

	tea "charm.land/bubbletea/v2"

	"llamadash/internal/collect"
)

// intervals are the refresh rates "+" and "-" step through.
var intervals = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second,
	2 * time.Second, 5 * time.Second, 10 * time.Second}

const defaultW, defaultH = 80, 24

// fetchTimeout bounds a history fetch: a database that does not answer shows
// "history unavailable" instead of freezing the charts on old data.
const fetchTimeout = 5 * time.Second

var errFetchTimeout = errors.New("history fetch timed out")

// Model is the Bubble Tea model of the live dashboard. It holds the latest
// snapshot (the live history lives in the collector's bounded rings) and, for a
// long range, the latest chart data read from the history database.
type Model struct {
	c        *collect.Collector
	opts     Options // the size, the range and the long-range chart data
	interval time.Duration
	snap     collect.Snapshot // the one displayed
	latest   collect.Snapshot // the latest poll result; differs from snap while paused
	paused   bool             // the display is frozen; polling and recording go on
	frozenAt time.Time        // the render clock while paused
	held     *longView        // a history result that arrived while paused, drawn on resume
	scales   Scales           // the chart tops of the session; opts.Scales is the copy drawn
	onScales func(Scales)     // told whenever a top went up (nil: nobody)
	polling  bool             // a Poll is in flight
	tickGen  int              // only the tick of the current generation triggers a poll

	src          SeriesSource   // nil: the live range only
	fetching     bool           // a history fetch, or a Series call it gave up on, is in flight
	pending      bool           // fetch again as soon as the one in flight returns
	longGen      int            // counts fetches started; only the latest one's refresh timer fires
	widths       map[string]int // chart plot widths of the latest fetch; nil: none for this range
	fetchTimeout time.Duration  // a fetch gives up after this (the hung call is waited for)

	setInterval func(time.Duration)                                  // tells the collector (tests replace it)
	after       func(time.Duration, func(time.Time) tea.Msg) tea.Cmd // tea.Tick (tests replace it)
	now         func() time.Time                                     // the render clock (tests replace it)
}

type pollMsg struct{ snap collect.Snapshot }

type tickMsg struct{ gen int }

// longMsg is the result of a history fetch. hung is set when the fetch gave up:
// it closes when the abandoned Series call has really returned.
type longMsg struct {
	rng  Range
	data map[string][]float64
	at   time.Time
	err  error
	hung <-chan struct{}
}

// longView is the long-range chart data as drawn.
type longView struct {
	data map[string][]float64
	at   time.Time
	err  bool
}

// setLong shows history data, or while paused holds it until the display is live
// again, so the frozen picture does not change.
func (m *Model) setLong(v longView) {
	if m.paused {
		m.held = &v
		return
	}
	m.opts.Long, m.opts.LongAt, m.opts.LongErr = v.data, v.at, v.err
}

// unhungMsg says the Series call a timed-out fetch left behind has returned.
type unhungMsg struct{}

// refetchMsg is the refresh timer set after history fetch gen returned.
type refetchMsg struct{ gen int }

// NewModel returns the dashboard model polling c every interval. o supplies the
// display mode and the starting range; the size comes from the terminal (80x24
// until it is known). src is the history database for the long ranges; without
// it the charts show the live range only.
func NewModel(c *collect.Collector, interval time.Duration, o Options, src SeriesSource) tea.Model {
	if o.Width <= 0 || o.Height <= 0 {
		o.Width, o.Height = defaultW, defaultH
	}
	o.History = src != nil
	if src == nil || o.Range < 0 || o.Range >= nRanges {
		o.Range = Range5m
	}
	o.Long, o.LongAt, o.LongErr = nil, time.Time{}, false
	return &Model{c: c, opts: o, interval: interval, src: src, fetchTimeout: fetchTimeout,
		scales: o.Scales, onScales: o.OnScales, setInterval: c.SetInterval, after: tea.Tick, now: time.Now}
}

// raiseScales lifts the session's chart tops to cover s and the history data long
// and reports a raise. It runs for every poll and fetch, paused or not, so what is
// remembered never misses a value; the tops drawn follow only while not paused.
func (m *Model) raiseScales(s collect.Snapshot, long map[string][]float64) {
	if !m.scales.raise(s, long) {
		return
	}
	if !m.paused {
		m.opts.Scales = m.scales
	}
	if m.onScales != nil {
		m.onScales(m.scales)
	}
}

// Init starts the first poll. A long starting range is fetched once the size or
// the first snapshot is known (see needFetch).
func (m *Model) Init() tea.Cmd { return m.poll() }

// poll starts a poll with the current interval. The interval reaches the
// collector in the same command, just before Poll, so changes apply in order.
func (m *Model) poll() tea.Cmd {
	m.polling = true
	c, set, d := m.c, m.setInterval, m.interval
	return func() tea.Msg {
		set(d)
		return pollMsg{snap: c.Poll(context.Background(), time.Now())}
	}
}

// tick schedules the next poll one interval from now. A new tick supersedes
// any tick still pending, so there is never more than one live chain.
func (m *Model) tick() tea.Cmd {
	m.tickGen++
	gen := m.tickGen
	return m.after(m.interval, func(time.Time) tea.Msg { return tickMsg{gen: gen} })
}

// step moves the interval dir steps through intervals (clamped at the ends). The
// collector gets the new interval with the next poll.
func (m *Model) step(dir int) tea.Cmd {
	i := 0
	for i < len(intervals)-1 && intervals[i] < m.interval {
		i++
	}
	m.interval = intervals[min(max(i+dir, 0), len(intervals)-1)]
	if !m.polling { // supersede the tick queued with the old interval
		return m.tick()
	}
	return nil
}

// needFetch starts a history fetch when a long range is shown and the charts'
// plot widths differ from those of the latest fetch (a resize, a chart that came
// or went, or none yet for this range). With a fetch in flight it only notes
// that another is due when that one returns.
func (m *Model) needFetch() tea.Cmd {
	if !m.opts.Range.long() {
		return nil
	}
	w := chartPlotWidths(m.snap, m.opts)
	if m.widths != nil && maps.Equal(w, m.widths) {
		return nil
	}
	if m.fetching {
		m.pending = true
		return nil
	}
	return m.fetch(w)
}

// fetch reads the current range for plot widths w in a command, off the UI
// thread: Series can take seconds on a busy database. After fetchTimeout the
// command gives up; the reads go on in their goroutine and their result is
// dropped.
func (m *Model) fetch(w map[string]int) tea.Cmd {
	m.fetching, m.pending, m.widths = true, false, w
	m.longGen++
	rng, src, to, limit := m.opts.Range, m.src, time.Now(), m.fetchTimeout
	return func() tea.Msg {
		res := make(chan longMsg, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			data, err := fetchLong(src, rng, to, w)
			res <- longMsg{rng: rng, data: data, at: to, err: err}
		}()
		t := time.NewTimer(limit)
		defer t.Stop()
		select {
		case r := <-res:
			return r
		case <-t.C:
			return longMsg{rng: rng, at: to, err: errFetchTimeout, hung: done}
		}
	}
}

// cycleRange moves to the next range; the old range's chart data is dropped, so
// the charts say "loading" until the new data arrives.
func (m *Model) cycleRange() tea.Cmd {
	m.opts.Range = m.opts.Range.next()
	m.opts.Long, m.opts.LongAt, m.opts.LongErr = nil, time.Time{}, false
	m.widths = nil
	return m.needFetch()
}

// gotLong takes a fetch result; one for another range is dropped. The next fetch
// starts at once when one is pending, else after the range's refresh period.
func (m *Model) gotLong(msg longMsg) tea.Cmd {
	m.raiseScales(collect.Snapshot{}, msg.data)
	if msg.hung != nil {
		// gave up: say so, but start nothing until the hung call returns, so there
		// is never more than one Series call in flight
		if msg.rng == m.opts.Range {
			m.setLong(longView{at: msg.at, err: true})
		}
		return func() tea.Msg { <-msg.hung; return unhungMsg{} }
	}
	return m.afterFetch(msg)
}

// afterFetch takes a fetch result, or the end of a hung call (a message for the
// current range with no time and no data), and starts or schedules the next fetch.
func (m *Model) afterFetch(msg longMsg) tea.Cmd {
	m.fetching = false
	if !m.opts.Range.long() {
		m.pending = false
		return nil
	}
	if msg.rng != m.opts.Range { // the range changed while it ran
		return m.fetch(chartPlotWidths(m.snap, m.opts))
	}
	if msg.at.IsZero() { // a hung call returned: its timeout was already shown
		if m.pending {
			return m.fetch(chartPlotWidths(m.snap, m.opts))
		}
		gen := m.longGen
		return m.after(m.opts.Range.refresh(), func(time.Time) tea.Msg { return refetchMsg{gen: gen} })
	}
	m.setLong(longView{data: msg.data, at: msg.at, err: msg.err != nil})
	if m.pending {
		return m.fetch(chartPlotWidths(m.snap, m.opts))
	}
	gen := m.longGen
	return m.after(m.opts.Range.refresh(), func(time.Time) tea.Msg { return refetchMsg{gen: gen} })
}

// Update handles keys, resizes, poll and history results and ticks.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "Q", "ctrl+c":
			return m, tea.Quit
		case "p", "P": // freezes the display only: polling, and so recording, go on
			// the picture is drawn against the time it was frozen, so nothing in it
			// turns stale or keeps counting
			m.paused = !m.paused
			if m.paused {
				m.frozenAt = m.now()
				return m, nil
			}
			m.snap, m.opts.Scales = m.latest, m.scales
			if m.held != nil {
				m.opts.Long, m.opts.LongAt, m.opts.LongErr = m.held.data, m.held.at, m.held.err
				m.held = nil
			}
			return m, m.needFetch() // the latest snapshot may show other charts
		case "+", "=":
			return m, m.step(-1)
		case "-", "_":
			return m, m.step(1)
		case "h", "H": // not while paused: the header says paused
			if m.src != nil && !m.paused {
				return m, m.cycleRange()
			}
		}
	case tea.WindowSizeMsg:
		m.opts.Width, m.opts.Height = msg.Width, msg.Height
		return m, m.needFetch()
	case pollMsg:
		m.latest, m.polling = msg.snap, false
		m.raiseScales(msg.snap, nil)
		if !m.paused {
			m.snap = msg.snap
		}
		return m, tea.Batch(m.tick(), m.needFetch())
	case longMsg:
		return m, m.gotLong(msg)
	case unhungMsg:
		return m, m.afterFetch(longMsg{rng: m.opts.Range})
	case refetchMsg:
		if msg.gen == m.longGen && !m.fetching && m.opts.Range.long() {
			return m, m.fetch(chartPlotWidths(m.snap, m.opts))
		}
	case tickMsg:
		if msg.gen != m.tickGen || m.polling {
			return m, nil
		}
		return m, m.poll()
	}
	return m, nil
}

// View renders the current snapshot on the alternate screen.
func (m *Model) View() tea.View {
	o := m.opts
	o.Paused, o.Interval, o.Now = m.paused, m.interval, m.now()
	if m.paused {
		o.Now = m.frozenAt
	}
	v := tea.NewView(Render(m.snap, o))
	v.AltScreen = true
	return v
}
