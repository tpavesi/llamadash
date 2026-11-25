package ui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"llamadash/internal/collect"
	"llamadash/internal/host"
	"llamadash/internal/stats"
)

const (
	recentMax = 10 // rows of the Recent requests panel
	recentMin = 3  // fewer rows than this and the panel is dropped

	containerEvery = 10 * time.Second // the collector's container lookup interval
)

// fmtUptime formats a duration coarsely: 42s, 44m, 3h12m, 2d4h (never negative).
func fmtUptime(d time.Duration) string {
	s := int(max(d, 0).Seconds())
	switch {
	case s >= 86400:
		return fmt.Sprintf("%dd%dh", s/86400, s%86400/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

// fmtDur formats a request duration: tenths of a second under 10s, else like fmtElapsed.
func fmtDur(d time.Duration, approx bool) string {
	if d < 10*time.Second {
		out := fmt.Sprintf("%.1fs", max(d, 0).Seconds())
		if approx {
			out = "~" + out
		}
		return out
	}
	return fmtElapsed(d, approx)
}

// fmtSize formats bytes with a 1024 base: 1.5T, 512.0G, 37G, 800M.
func fmtSize(b uint64) string {
	f := float64(b)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.1fT", f/(1<<40))
	case f >= 100<<30:
		return fmt.Sprintf("%.0fG", f/(1<<30))
	case f >= 1<<30:
		return fmt.Sprintf("%.1fG", f/(1<<30))
	}
	return fmt.Sprintf("%.0fM", f/(1<<20))
}

// kbGB formats kilobytes as GB (1024 base) with one decimal.
func kbGB(kb uint64) string { return fmt.Sprintf("%.1f", float64(kb)/(1<<20)) }

// now is the render time: Options.Now, or the snapshot's poll time.
func (r *renderer) now() time.Time {
	if !r.o.Now.IsZero() {
		return r.o.Now
	}
	return r.s.At
}

// fmtWhen is a finished request's time: 15:04:05 on the snapshot's local day (now),
// 01-02 15:04 before it (rows are seeded from up to 90 days of stored requests).
func fmtWhen(at, now time.Time) string {
	t := at.Local()
	if !now.IsZero() {
		y, m, d := now.Local().Date()
		if t.Before(time.Date(y, m, d, 0, 0, 0, 0, time.Local)) {
			return t.Format("01-02 15:04")
		}
	}
	return t.Format("15:04:05")
}

// Recent requests table columns, in display order.
const (
	rTime = iota
	rSlot
	rPromptC
	rCached
	rGenerated
	rDuration
	rRate
	rTTFT
	nRecentCols
)

var (
	recentTitle = [nRecentCols]string{"time", "slot", "prompt", "cached", "generated", "duration", "tok/s", "ttft"}
	recentWidth = [nRecentCols]int{11, 4, 7, 6, 9, 8, 6, 7}         // time: "01-02 15:04" for earlier days
	recentDrop  = []int{rCached, rTTFT, rSlot, rDuration, rPromptC} // dropped first when narrow
)

// recentLayout returns the shown columns and the gap between them for iw cells.
func recentLayout(iw int) (cols []int, gap int) {
	shown := [nRecentCols]bool{}
	for c := range shown {
		shown[c] = true
	}
	need := func() int {
		n, total := 0, 0
		for c, ok := range shown {
			if ok {
				n++
				total += recentWidth[c]
			}
		}
		return total + 2*max(n-1, 0)
	}
	for _, c := range recentDrop {
		if need() <= iw {
			break
		}
		shown[c] = false
	}
	for c, ok := range shown {
		if ok {
			cols = append(cols, c)
		}
	}
	gap = 2
	if extra := iw - need(); extra > 0 && len(cols) > 1 {
		gap += min(extra/(len(cols)-1), 2) // wider gaps make the rows hard to follow
	}
	return cols, gap
}

// recentPanel is the Recent requests panel with n rows, newest first.
func (r *renderer) recentPanel(iw, n int) panel {
	cols, gap := recentLayout(iw)
	row := func(cells [nRecentCols]string) string {
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			if c == rTime {
				parts = append(parts, fit(cells[c], recentWidth[c]))
			} else {
				parts = append(parts, padLeft(cells[c], recentWidth[c]))
			}
		}
		return strings.Join(parts, strings.Repeat(" ", gap))
	}
	lines := []string{r.paint(rDim, row(recentTitle))}
	for _, f := range r.s.Recent[:min(n, len(r.s.Recent))] {
		lines = append(lines, row(r.recentCells(f)))
	}
	const title = "Recent requests"
	tag := "newest first, ~ approximate, - not measured"
	// the box head is " title ─ tag " in iw+2 cells, with at least one ─ after it
	if width(title)+width(tag)+6 > iw+2 {
		tag = "newest first"
	}
	return panel{title: title, tag: tag, tagRole: rDim, lines: lines}
}

func (r *renderer) recentCells(f stats.Finished) [nRecentCols]string {
	var c [nRecentCols]string
	c[rTime] = r.paint(rLabel, fmtWhen(f.At, r.s.At))
	c[rSlot] = fmt.Sprint(f.SlotID)
	c[rPromptC] = r.paint(rValue, fmtCount(f.PromptTokens))
	c[rCached] = fmt.Sprintf("%.0f%%", clamp01(f.CacheShare)*100)
	c[rGenerated] = r.paint(rValue, fmtCount(f.Generated))
	c[rDuration] = fmtDur(f.Duration, f.Approx)
	c[rRate], c[rTTFT] = r.paint(rDim, "-"), r.paint(rDim, "-")
	if f.GenRate > 0 {
		c[rRate] = r.paint(rGen, fmtRate(f.GenRate))
	}
	if f.TTFT > 0 {
		c[rTTFT] = fmtDur(f.TTFT, f.Approx)
	}
	return c
}

// hostPanel shows the host's CPU, memory, disk, CPU temperature and fans (nil
// Host: no panel). The fan and temperature readings share its stale tag.
func (r *renderer) hostPanel(iw int) panel {
	h := *r.s.Host
	at := r.s.HostAt
	if (len(r.s.Fans) > 0 || r.s.HasCPUTemp) && r.s.HwmonAt.Before(at) {
		at = r.s.HwmonAt
	}
	return r.staled(panel{title: "Host", lines: r.hostLines(h, iw)}, r.staleTag(at, r.o.Interval))
}

// fanRank orders the fans row: stopped, then missing (n/a), then running.
func fanRank(f collect.Fan) int {
	switch {
	case f.Stopped:
		return 0
	case f.Missing:
		return 1
	}
	return 2
}

// shownFans returns the fans in display order (fanRank, else as given) with
// their names: the label, prefixed by the chip name when the fans are on more
// than one chip.
func shownFans(fs []collect.Fan) ([]collect.Fan, []string) {
	out := slices.Clone(fs)
	slices.SortStableFunc(out, func(a, b collect.Fan) int { return fanRank(a) - fanRank(b) })
	multi := false
	for _, f := range out {
		multi = multi || f.Chip != out[0].Chip
	}
	names := make([]string, len(out))
	for i, f := range out {
		names[i] = ascii(f.Label)
		if multi && f.Chip != "" {
			names[i] = ascii(f.Chip) + " " + names[i]
		}
	}
	return out, names
}

// cpuTemp is the CPU temperature as the GPU panel prints its own: amber from
// 85°C, red from 95°C (on the value shown), "!"/"!!" without colour.
func (r *renderer) cpuTemp() string {
	t := math.Round(r.s.CPUTempC)
	tr := rValue
	switch {
	case t >= 95:
		tr = rBad
	case t >= 85:
		tr = rWarn
	}
	return r.paint(rLabel, "temp ") + r.paint(tr, fmt.Sprintf("%.0f%sC", t, r.g.deg)+r.warnMark(tr))
}

// fansRow is "fans   <label> <rpm>   ...   rpm" in iw cells: as many fans as fit,
// then "+N" for the rest. A stopped fan is red ("!!" without colour), a missing
// one "<label> n/a" in amber ("!").
func (r *renderer) fansRow(iw, labelW int) string {
	fans, names := shownFans(r.s.Fans)
	const sep = "   "
	parts := make([]string, len(fans))
	for i, f := range fans {
		rpm := strconv.Itoa(f.RPM)
		switch {
		case f.Stopped:
			parts[i] = r.paint(rBad, names[i]+" "+rpm+r.warnMark(rBad))
		case f.Missing:
			parts[i] = r.paint(rWarn, names[i]+" n/a"+r.warnMark(rWarn))
		default:
			parts[i] = r.paint(rLabel, names[i]+" ") + r.paint(rValue, rpm)
		}
	}
	head, tail := fit(r.paint(rLabel, "fans"), labelW), r.paint(rLabel, "rpm")
	var row string
	for n := len(parts); n >= 0; n-- {
		shown := slices.Clone(parts[:n])
		if n < len(parts) {
			shown = append(shown, r.paint(rLabel, "+"+strconv.Itoa(len(parts)-n)))
		}
		row = head + strings.Join(append(shown, tail), sep)
		if width(row) <= iw {
			break
		}
	}
	return row
}

func (r *renderer) hostLines(h host.Info, iw int) []string {
	const labelW = 7
	paint := r.paint
	used := h.MemTotalKB - min(h.MemAvailKB, h.MemTotalKB)
	memFrac, memRole := 0.0, rMem
	if h.MemTotalKB > 0 {
		memFrac = float64(used) / float64(h.MemTotalKB)
	}
	if memFrac >= 0.9 {
		memRole = rWarn
	}
	// without colour the RAM warning is the "!" the GPU rows use
	ram := kbGB(used) + "/" + kbGB(h.MemTotalKB) + " GB" + r.warnMark(memRole)
	// as the GPU panel, so the bars line up, unless the RAM figure needs more
	valW := max(13, width(ram))
	cpuTail := paint(rValue, fmt.Sprint(h.Cores)) + paint(rLabel, " cores   load ") +
		paint(rValue, fmt.Sprintf("%.2f %.2f %.2f", h.Load1, h.Load5, h.Load15))
	if r.s.HasCPUTemp {
		cpuTail += "   " + r.cpuTemp()
	}
	swap := paint(rLabel, "swap ") + paint(rValue, kbGB(h.SwapTotalKB-min(h.SwapFreeKB, h.SwapTotalKB))+"/"+kbGB(h.SwapTotalKB)+" GB")
	if h.SwapTotalKB == 0 {
		swap = paint(rDim, "no swap")
	}
	// one bar width for both lines, leaving room for the longer tail
	barW := min(iw-labelW-valW-2-3-max(width(cpuTail), width(swap)), maxGauge)
	line := func(label, val string, valRole role, frac float64, fill role, ok bool, tail string) string {
		l := fit(paint(rLabel, label), labelW)
		if !ok {
			l += fit(paint(rDim, "n/a"), valW)
		} else {
			l += fit(paint(valRole, val), valW)
		}
		if barW >= 4 {
			if ok {
				l += "  " + r.bar(frac, barW, fill, paint)
			} else {
				l += strings.Repeat(" ", barW+2)
			}
		}
		return l + "   " + tail
	}
	lines := []string{
		line("cpu", fmt.Sprintf("%.0f%%", h.CPUPct), rValue, h.CPUPct/100, rGood, h.HasCPU, cpuTail),
		line("ram", ram, memRole, memFrac, memRole, h.MemTotalKB > 0, swap),
	}
	if h.HasDisk {
		lines = append(lines, fit(paint(rLabel, "disk /"), labelW)+paint(rValue, fmtSize(h.DiskFree))+
			paint(rLabel, " free of ")+paint(rValue, fmtSize(h.DiskTotal)))
	}
	if len(r.s.Fans) > 0 {
		lines = append(lines, r.fansRow(iw, labelW))
	}
	return lines
}

// serverPanel shows the llama-server process and its container. Each line is
// dimmed on its own when its source is stale; the tag names the stale one.
func (r *renderer) serverPanel() panel {
	s := r.s
	const labelW = 10
	paint := r.paint
	label := func(l string) string { return fit(paint(rLabel, l), labelW) }
	sep := "   "
	var lines []string
	var tags []string
	var stale []bool
	if p := s.Proc; p != nil {
		cpu := paint(rDim, "n/a")
		if p.HasCPU {
			cpu = paint(rValue, fmt.Sprintf("%.0f%%", p.CPUPct)) + paint(rLabel, " (of one core)")
		}
		l := label("process") + paint(rLabel, "pid ") + paint(rValue, fmt.Sprint(p.PID)) + sep +
			paint(rLabel, "cpu ") + cpu + sep + paint(rLabel, "mem ") + paint(rValue, kbGB(p.RSSKB)+" GB") + sep +
			paint(rLabel, "threads ") + paint(rValue, fmt.Sprint(p.Threads))
		lines = append(lines, l)
		tag := r.staleTag(s.ProcAt, r.o.Interval)
		tags, stale = append(tags, tag), append(stale, tag != "")
	}
	if c := s.Container; c != nil {
		state := ascii(c.State)
		stRole := rGood
		if state != "running" {
			stRole = rBad
		}
		health, hRole := ascii(c.Health), rValue
		switch c.Health {
		case "":
			health, hRole = "no healthcheck", rDim
		case "healthy":
			hRole = rGood
		case "unhealthy":
			hRole = rBad
		case "starting":
			hRole = rWarn
		}
		// without colour, starting and unhealthy carry the ! / !! markers
		l := label("container") + paint(rValue, ascii(c.Name)) + sep + paint(stRole, state) + sep +
			paint(hRole, health+r.warnMark(hRole))
		if c.State == "running" && !c.StartedAt.IsZero() {
			l += sep + paint(rLabel, "up ") + paint(rValue, fmtUptime(r.now().Sub(c.StartedAt)))
		}
		l += sep + paint(rLabel, "restarts ") + paint(rValue, fmt.Sprint(c.Restarts))
		lines = append(lines, l)
		tag := r.staleTag(s.ContainerAt, max(r.o.Interval, containerEvery))
		tags, stale = append(tags, tag), append(stale, tag != "")
	}
	p := panel{title: "llama-server", lines: lines}
	switch {
	case len(lines) == 2 && stale[0] && stale[1]:
		oldest := s.ProcAt
		if s.ContainerAt.Before(oldest) {
			oldest = s.ContainerAt
		}
		return r.staled(p, r.staleTag(oldest, r.o.Interval))
	case len(lines) == 1 && stale[0]:
		return r.staled(p, tags[0])
	}
	for i, st := range stale {
		if st {
			p.lines[i] = paint(rDim, ansi.Strip(p.lines[i]))
			name := "process"
			if s.Proc == nil || i == 1 {
				name = "container"
			}
			p.tag, p.tagRole = name+" "+tags[i], rWarn
		}
	}
	return p
}
