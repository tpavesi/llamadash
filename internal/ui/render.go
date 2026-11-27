package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"llamadash/internal/collect"
	"llamadash/internal/llama"
)

// Options controls Render.
type Options struct {
	Width, Height int
	ASCII         bool // only 7-bit characters
	Color16       bool // 16-colour palette and console-font glyphs (TERM=linux)
	NoColor       bool // NO_COLOR set: no escape sequences at all
	Paused        bool
	Interval      time.Duration
	Now           time.Time

	Range   Range                // what the charts show
	History bool                 // a history database is there: the h key cycles Range
	Long    map[string][]float64 // long ranges: chart key -> one value per chart column, NaN = no data
	LongAt  time.Time            // when Long was fetched; zero: not yet (the charts say "loading")
	LongErr bool                 // the fetch failed: the charts say "history unavailable"

	Scales   Scales       // the learned chart tops to start from (the model raises them)
	OnScales func(Scales) // the live model calls it from Update whenever a top went up; must not block
}

// DetectOptions derives the display mode from the environment: TERM=linux selects
// the 16-colour palette and console-safe glyphs, NO_COLOR disables colour.
func DetectOptions(env func(string) string) Options {
	return Options{Color16: env("TERM") == "linux", NoColor: env("NO_COLOR") != ""}
}

// glyphs is the character set for one display mode.
type glyphs struct {
	h, v, tl, tr, bl, br string
	full, empty          string
	spark                []string // nil: no sparklines, a bar of the latest value instead
	more, deg, times     string
	sep                  string
}

var (
	unicodeGlyphs = glyphs{h: "─", v: "│", tl: "┌", tr: "┐", bl: "└", br: "┘", full: "█", empty: "░",
		spark: strings.Split("▁▂▃▄▅▆▇█", ""), more: "…", deg: "°", times: "×", sep: "│"}
	// consoleGlyphs only uses characters of the Linux console's default (CP437) font.
	consoleGlyphs = glyphs{h: "─", v: "│", tl: "┌", tr: "┐", bl: "└", br: "┘", full: "█", empty: "░",
		more: "...", deg: "°", times: "x", sep: "│"}
	asciiGlyphs = glyphs{h: "-", v: "|", tl: "+", tr: "+", bl: "+", br: "+", full: "#", empty: ".",
		more: "...", times: "x", sep: "|"}
)

// role is the meaning of a piece of text; the palette maps it to a colour.
type role int

const (
	rBorder role = iota
	rTitle
	rLabel
	rValue
	rDim
	rGood
	rWarn
	rBad
	rGen
	rPrompt
	rPower
	rMem
	rKey
	rTrack
	rPaused
	nRoles
)

// palette16 uses only the 16 ANSI colours and avoids dark blue, which is
// unreadable on the Linux console's black background.
var palette16 = [nRoles]color.Color{
	rBorder: lipgloss.Cyan, rTitle: lipgloss.BrightWhite, rLabel: lipgloss.White, rValue: lipgloss.BrightWhite,
	rDim: lipgloss.BrightBlack, rGood: lipgloss.BrightGreen, rWarn: lipgloss.BrightYellow, rBad: lipgloss.BrightRed,
	rGen: lipgloss.BrightGreen, rPrompt: lipgloss.BrightCyan, rPower: lipgloss.BrightYellow, rMem: lipgloss.BrightMagenta,
	rKey: lipgloss.BrightCyan, rTrack: lipgloss.BrightBlack, rPaused: lipgloss.BrightYellow,
}

var palette256 = [nRoles]color.Color{
	rBorder: lipgloss.ANSIColor(66), rTitle: lipgloss.ANSIColor(255), rLabel: lipgloss.ANSIColor(250),
	rValue: lipgloss.ANSIColor(231), rDim: lipgloss.ANSIColor(243), rGood: lipgloss.ANSIColor(114),
	rWarn: lipgloss.ANSIColor(221), rBad: lipgloss.ANSIColor(203), rGen: lipgloss.ANSIColor(114),
	rPrompt: lipgloss.ANSIColor(80), rPower: lipgloss.ANSIColor(214), rMem: lipgloss.ANSIColor(176),
	rKey: lipgloss.ANSIColor(80), rTrack: lipgloss.ANSIColor(238), rPaused: lipgloss.ANSIColor(221),
}

type renderer struct {
	s  collect.Snapshot
	o  Options
	g  glyphs
	st [nRoles]lipgloss.Style
}

func newRenderer(s collect.Snapshot, o Options) *renderer {
	r := &renderer{s: s, o: o, g: unicodeGlyphs}
	switch {
	case o.ASCII:
		r.g = asciiGlyphs
	case o.Color16:
		r.g = consoleGlyphs
	}
	pal := palette256
	if o.Color16 {
		pal = palette16
	}
	for i := range r.st {
		r.st[i] = lipgloss.NewStyle().Foreground(pal[i])
	}
	r.st[rTitle] = r.st[rTitle].Bold(true)
	r.st[rKey] = r.st[rKey].Bold(true)
	r.st[rPaused] = r.st[rPaused].Bold(true).Reverse(true)
	return r
}

// paint colours s according to its role (nothing with NoColor).
func (r *renderer) paint(ro role, s string) string {
	if r.o.NoColor || s == "" {
		return s
	}
	return r.st[ro].Render(s)
}

// width is the display width of s, ignoring escape sequences.
func width(s string) int { return lipgloss.Width(s) }

// fit truncates or pads s to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) > w {
		s = ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", max(0, w-width(s)))
}

// padLeft right-aligns s in w cells (s is truncated if longer).
func padLeft(s string, w int) string {
	if width(s) >= w {
		return fit(s, w)
	}
	return strings.Repeat(" ", w-width(s)) + s
}

// wrap breaks plain text into lines of at most w cells at spaces.
func wrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case width(line)+1+width(word) <= w:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

func fmtAge(d time.Duration) string {
	s := int(max(d, 0).Seconds())
	switch {
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

func fmtRate(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

// staleTag returns "stale, Ns ago" when at is older than three intervals.
func (r *renderer) staleTag(at time.Time, interval time.Duration) string {
	if at.IsZero() || r.o.Now.IsZero() {
		return ""
	}
	if age := r.o.Now.Sub(at); age > 3*interval {
		return "stale, " + fmtAge(age) + " ago"
	}
	return ""
}

// staled marks p as stale when tag is set: the age replaces the title tag and
// the content is dimmed as a whole (with NoColor the tag alone carries it).
func (r *renderer) staled(p panel, tag string) panel {
	if tag == "" {
		return p
	}
	p.tag, p.tagRole = tag, rWarn
	lines := make([]string, len(p.lines))
	for i, l := range p.lines {
		lines[i] = r.paint(rDim, ansi.Strip(l))
	}
	p.lines = lines
	return p
}

// serverStale is the stale tag of everything derived from /slots.
func (r *renderer) serverStale() string { return r.staleTag(r.s.SlotsAt, r.o.Interval) }

// gpuStale is the stale tag of the GPU readings (polled every 2s at most).
func (r *renderer) gpuStale() string {
	return r.staleTag(r.s.GPUAt, max(r.o.Interval, 2*time.Second))
}

// energyStale follows the GPU readings. CPU-only power has no timestamp in the
// snapshot, so without GPUs the Energy panel is treated as fresh.
func (r *renderer) energyStale() string {
	if len(r.s.GPUs) == 0 {
		return ""
	}
	return r.gpuStale()
}

// Render draws the whole screen for s at o.Width x o.Height. Every line fits the
// width and there are never more than o.Height lines.
func Render(s collect.Snapshot, o Options) string {
	if o.Width <= 0 || o.Height <= 0 {
		return ""
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	r := newRenderer(s, o)
	return strings.Join(r.screen(), "\n")
}

func (r *renderer) screen() []string {
	W, H := r.o.Width, r.o.Height
	out := []string{r.header()}
	if H == 1 {
		return out
	}
	status := r.status()
	out = append(out, status[:min(len(status), H-2)]...)
	out = append(out, r.body(r.bodyAvail())...)
	for len(out) < H-1 {
		out = append(out, "")
	}
	for i := range out {
		out[i] = fit(out[i], W)
	}
	return append(out, fit(r.footer(), W))
}

func (r *renderer) header() string {
	s, g := r.s, r.g
	sep := " " + r.paint(rDim, g.sep) + " "
	model := "model unknown"
	ctx, quant := "", ""
	if p := s.Props; p != nil {
		if p.ModelAlias != "" {
			model = ascii(p.ModelAlias)
		}
		quant = llama.ShortFtype(p.ModelFtype) // the quantisation, e.g. Q4_K_M
		slots := "slots"
		if p.TotalSlots == 1 {
			slots = "slot"
		}
		ctx = fmt.Sprintf("ctx %s %s %d %s", fmtCount(p.NCtx), g.times, p.TotalSlots, slots)
	}
	health, hr := healthOf(s)
	W := r.o.Width
	note := s.DBStatus == "" && s.DBReadOnly // another llamadash is recording: not a fault, the normal colour

	// What is shown when the line is tight. Always: the name, the health (never
	// cut), a database error, "db: read-only" and "paused". Then, most important
	// first, while they fit: the model (cut to the room left), the context size, a
	// long range, the long read-only note, the long paused form, the refresh rate,
	// "range live", the size. The first that does not fit ends the list, so when
	// tight the size goes first, then "range live", the refresh rate, the long
	// paused form, the long note. The quantisation after the model name comes last
	// of all, so it is the very first to go.
	var show struct {
		model                                                            string
		ctx, longRange, noteLong, pausedLong, refresh, live, size, quant bool
	}
	build := func() (left, right string) {
		left = r.paint(rTitle, "llamadash")
		if show.model != "" {
			left += sep + r.paint(rValue, show.model)
			if show.quant && quant != "" && show.model == model {
				left += " " + r.paint(rLabel, quant)
			}
		}
		if ctx != "" && show.ctx {
			left += sep + r.paint(rLabel, ctx)
		}
		left += sep + r.paint(hr, health)
		switch {
		case s.DBStatus != "":
			left += sep + r.paint(rWarn, "db: "+ascii(s.DBStatus))
		case note && show.noteLong:
			left += sep + r.paint(rLabel, "db: read-only (another llamadash is recording)")
		case note:
			left += sep + r.paint(rLabel, "db: read-only")
		}
		var parts []string
		if r.o.Paused { // the display only: polling and recording go on
			p := " paused "
			if r.o.History && !s.DBReadOnly && show.pausedLong {
				p = " paused (still recording) "
			}
			parts = append(parts, r.paint(rPaused, p))
		}
		switch {
		case r.o.Range.long() && show.longRange:
			parts = append(parts, r.paint(rLabel, "range "+r.o.Range.String()))
		case !r.o.Range.long() && r.o.History && show.live:
			parts = append(parts, r.paint(rLabel, "range live"))
		}
		if show.refresh {
			parts = append(parts, r.paint(rLabel, "refresh "+r.o.Interval.String()))
		}
		if show.size {
			parts = append(parts, r.paint(rDim, fmt.Sprintf("%dx%d", r.o.Width, r.o.Height)))
		}
		return left, strings.Join(parts, sep)
	}
	fits := func() bool {
		l, rt := build()
		return width(l)+2+width(rt) <= W
	}
	if show.model = model; !fits() {
		show.model = ""
		l, rt := build()
		if room := W - width(l) - 2 - width(rt) - width(sep); room >= 4 {
			show.model = ansi.Truncate(model, room, "")
		}
	}
	for _, f := range []*bool{&show.ctx, &show.longRange, &show.noteLong, &show.pausedLong, &show.refresh,
		&show.live, &show.size, &show.quant} {
		if *f = true; !fits() {
			*f = false
			break
		}
	}
	left, right := build()
	lw, rw := width(left), width(right)
	if lw+2+rw <= W {
		return left + strings.Repeat(" ", W-lw-rw) + right
	}
	return fit(left, W) // only a very narrow terminal or a very long health text

}

// status explains an unreachable or loading server, above the panels.
func (r *renderer) status() []string {
	s := r.s
	var msg string
	ro := rBad
	switch {
	case s.At.IsZero():
		return nil
	case !s.Reachable:
		since := "unknown"
		if !s.UnreachableSince.IsZero() {
			since = s.UnreachableSince.Format("15:04:05") + " (" + fmtAge(s.At.Sub(s.UnreachableSince)) + ")"
		}
		msg = fmt.Sprintf("server %s unreachable since %s, retrying", ascii(s.URL), since)
	case s.Loading:
		msg, ro = "server is loading the model", rWarn
	default:
		return nil
	}
	ls := wrap(msg, r.o.Width)
	if len(ls) > 3 {
		ls = ls[:3]
	}
	for i := range ls {
		ls[i] = r.paint(ro, ls[i])
	}
	return ls
}

func (r *renderer) footer() string {
	keys := "q quit   p pause   +/- refresh rate"
	if r.o.History {
		keys = "q quit   p pause   +/- refresh   h range"
	}
	left := r.paint(rLabel, keys)
	url := ascii(r.s.URL)
	if url != "" && width(left)+4+width(url) <= r.o.Width {
		return left + strings.Repeat(" ", r.o.Width-width(left)-width(url)) + r.paint(rDim, url)
	}
	return left
}

// panel is a titled box's content; lines are fitted to the inner width.
type panel struct {
	title, tag string
	tagRole    role
	lines      []string
}

// box draws p w cells wide and h rows tall (h <= 0: as tall as its content).
func (r *renderer) box(p panel, w, h int) []string {
	if w <= 0 {
		return nil
	}
	if h <= 0 {
		h = len(p.lines) + 2
	}
	if w < 4 || h < 2 {
		out := append([]string{p.title}, p.lines...)
		out = out[:min(len(out), h)]
		for i := range out {
			out[i] = fit(out[i], w)
		}
		return out
	}
	g := r.g
	head := " " + r.paint(rTitle, p.title) + " "
	if p.tag != "" {
		head += r.paint(rBorder, g.h) + " " + r.paint(p.tagRole, p.tag) + " "
	}
	if width(head) > w-2 {
		head = fit(head, w-2)
	}
	out := []string{r.paint(rBorder, g.tl) + head + r.paint(rBorder, strings.Repeat(g.h, w-2-width(head))+g.tr)}
	side := r.paint(rBorder, g.v)
	for i := 0; i < h-2; i++ {
		l := ""
		if i < len(p.lines) {
			l = p.lines[i]
		}
		out = append(out, side+" "+fit(l, w-4)+" "+side)
	}
	return append(out, r.paint(rBorder, g.bl+strings.Repeat(g.h, w-2)+g.br))
}
