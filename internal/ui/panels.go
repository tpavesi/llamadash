package ui

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"llamadash/internal/collect"
	"llamadash/internal/cost"
	"llamadash/internal/gpu"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

// Requests table columns, in display order.
const (
	cSlot = iota
	cState
	cCtx
	cPrompt
	cGen
	cRate
	cElapsed
	nCols
)

var (
	colTitle = [nCols]string{"slot", "state", "ctx used", "prompt", "generated", "tok/s", "elapsed"}
	colWidth = [nCols]int{4, 11, 0, 20, 9, 7, 8} // cCtx is bar + 1 + ctxNumW
	colRight = [nCols]bool{true, false, false, true, true, true, true}
	dropCols = []int{cPrompt, cElapsed, cGen, cCtx, cRate} // dropped first when narrow
)

const (
	ctxNumW = 6
	minBar  = 6
)

type tableLayout struct {
	cols []int // shown columns
	bar  int   // ctx bar cells
	gap  int
	w    [nCols]int
}

// layoutTable fits the columns into iw cells: drops columns when narrow and
// gives extra room to the ctx bar (up to maxGauge), then a little to the gaps.
func layoutTable(iw int) tableLayout {
	t := tableLayout{gap: 2, bar: minBar, w: colWidth}
	shown := map[int]bool{}
	for c := 0; c < nCols; c++ {
		shown[c] = true
	}
	need := func() int {
		n, total := 0, 0
		for c := 0; c < nCols; c++ {
			if shown[c] {
				n++
				if c == cCtx {
					total += minBar + 1 + ctxNumW
				} else {
					total += colWidth[c]
				}
			}
		}
		return total + 2*max(n-1, 0)
	}
	for _, c := range dropCols {
		if need() <= iw {
			break
		}
		delete(shown, c)
	}
	for c := 0; c < nCols; c++ {
		if shown[c] {
			t.cols = append(t.cols, c)
		}
	}
	extra := iw - need()
	if shown[cCtx] && extra > 0 {
		add := min(extra, maxGauge-minBar)
		t.bar += add
		extra -= add
	}
	if extra > 0 && len(t.cols) > 1 {
		t.gap += min(extra/(len(t.cols)-1), 4)
	}
	t.w[cCtx] = t.bar + 1 + ctxNumW
	return t
}

func (t tableLayout) row(cells [nCols]string) string {
	var parts []string
	for _, c := range t.cols {
		if colRight[c] {
			parts = append(parts, padLeft(cells[c], t.w[c]))
		} else {
			parts = append(parts, fit(cells[c], t.w[c]))
		}
	}
	return strings.Join(parts, strings.Repeat(" ", t.gap))
}

func stateRole(st stats.State) role {
	switch st {
	case stats.Generating:
		return rGen
	case stats.PromptEval:
		return rPrompt
	}
	return rDim
}

// slotRow renders one slot.
func (r *renderer) slotRow(t tableLayout, v stats.SlotView) string {
	paint := r.paint
	var c [nCols]string
	c[cSlot] = fmt.Sprint(v.ID)
	c[cState] = paint(stateRole(v.State), v.State.String())
	if v.State != stats.Idle {
		frac := 0.0
		if v.NCtx > 0 {
			frac = float64(v.CtxUsed) / float64(v.NCtx)
		}
		fill := rPrompt
		switch {
		case frac >= 0.95:
			fill = rBad
		case frac >= 0.8:
			fill = rWarn
		}
		c[cCtx] = r.bar(frac, t.bar, fill, paint) + " " + paint(rValue, padLeft(fmtCount(v.CtxUsed), ctxNumW))
		c[cPrompt] = fmt.Sprintf("%s (%.0f%% cached)", fmtCount(promptCount(v)), v.CacheShare*100)
		c[cGen] = paint(rValue, fmtCount(v.Decoded))
		rate := v.GenRate
		if v.State == stats.PromptEval {
			rate = v.PromptRate
		}
		c[cRate] = paint(rValue, fmtRate(rate))
		c[cElapsed] = fmtElapsed(v.Elapsed, v.ElapsedApprox)
	}
	return t.row(c)
}

// requestsLines is the Requests panel content in at most maxInner lines (< 0: no limit).
func (r *renderer) requestsLines(iw, maxInner int) []string {
	s := r.s
	var queued string
	if q, ok := s.Metrics["requests_deferred"]; ok {
		queued = r.paint(rLabel, "queued: ") + r.paint(rValue, fmt.Sprint(int(q)))
		if p, ok := s.Metrics["requests_processing"]; ok {
			queued += r.paint(rLabel, "   processing: ") + r.paint(rValue, fmt.Sprint(int(p)))
		}
	}
	var out []string
	switch {
	case s.SlotsHint != "":
		for _, l := range wrap(ascii(s.SlotsHint), iw) {
			out = append(out, r.paint(rWarn, l))
		}
	case len(s.Slots) == 0:
		msg := "no slot data"
		switch {
		case s.At.IsZero():
			msg = "waiting for the first poll"
		case !s.Reachable:
			msg = "no data while the server is unreachable"
		}
		out = append(out, r.paint(rDim, msg))
	default:
		budget := maxInner
		if queued != "" {
			budget--
		}
		if maxInner >= 0 { // a limit of 0 must not become "no limit"
			budget = max(budget, 0)
		}
		out = r.table(iw, budget)
	}
	if queued != "" {
		out = append(out, queued)
	}
	return out
}

// requestsHeight is len(r.requestsLines(iw, -1)) without rendering the slot rows.
func (r *renderer) requestsHeight(iw int) int {
	s := r.s
	n := 0
	if _, ok := s.Metrics["requests_deferred"]; ok {
		n++ // the queued line
	}
	switch {
	case s.SlotsHint != "":
		return n + len(wrap(ascii(s.SlotsHint), iw))
	case len(s.Slots) == 0:
		return n + 1
	}
	return n + 1 + len(s.Slots) // column header and one row per slot
}

// table renders the column header and slot rows in at most budget lines (< 0: all).
func (r *renderer) table(iw, budget int) []string {
	s := r.s
	t := layoutTable(iw)
	slots := s.Slots
	more := 0
	if budget >= 0 && len(slots)+1 > budget {
		busy := 0
		for _, v := range slots {
			if v.State != stats.Idle {
				busy++
			}
		}
		if budget < 2 {
			return []string{fmt.Sprintf("%d slots, %d busy", len(slots), busy)}
		}
		keep := budget - 2 // header and the "more" line
		// show busy slots first, then idle ones, in slot order
		order := slices.Clone(slots)
		sort.SliceStable(order, func(i, j int) bool {
			return order[i].State != stats.Idle && order[j].State == stats.Idle
		})
		slots = order[:keep]
		sort.SliceStable(slots, func(i, j int) bool { return slots[i].ID < slots[j].ID })
		more = len(s.Slots) - keep
	}
	var titles [nCols]string
	for c := range titles {
		titles[c] = colTitle[c]
	}
	out := []string{r.paint(rDim, t.row(titles))}
	for _, v := range slots {
		out = append(out, r.slotRow(t, v))
	}
	if more > 0 {
		out = append(out, r.paint(rDim, fmt.Sprintf("%s %d more", r.g.more, more)))
	}
	return out
}

// throughputPanel shows generation and prompt rates with their trend, and the
// server's token totals when /metrics is available.
func (r *renderer) throughputPanel(iw int) panel {
	s := r.s
	const labelW, valW = 8, 12
	row := func(label string, v float64, hist []float64, ro role, key string) string {
		l := fit(r.paint(rLabel, label), labelW) + r.paint(ro, padLeft(fmtRate(v)+" tok/s", valW))
		if rest := iw - labelW - valW - 2; rest >= 4 {
			l += "  " + r.trend(hist, rest, ro, r.top(key)) // the chart's top
		}
		return l
	}
	lines := []string{
		row("gen", s.GenRate, s.GenHist, rGen, "gen_rate"),
		row("prompt", s.PromptRate, s.PromptHist, rPrompt, "prompt_rate"),
	}
	gen, okG := s.Metrics["tokens_predicted_total"]
	pr, okP := s.Metrics["prompt_tokens_total"]
	switch {
	case okG || okP:
		var parts []string
		if okG {
			parts = append(parts, r.paint(rValue, fmtCount(int(gen)))+r.paint(rLabel, " generated"))
		}
		if okP {
			parts = append(parts, r.paint(rValue, fmtCount(int(pr)))+r.paint(rLabel, " prompt"))
		}
		lines = append(lines, fit(r.paint(rLabel, "total"), labelW)+strings.Join(parts, "   "))
	case s.MetricsHint != "":
		for _, l := range wrap(ascii(s.MetricsHint), iw) {
			lines = append(lines, r.paint(rDim, l))
		}
	}
	return r.staled(panel{title: "Throughput", lines: lines}, r.serverStale())
}

// gpuPanels returns one panel builder per GPU, in index order.
func (r *renderer) gpuPanels() []func(int) panel {
	s := r.s
	gpus := slices.Clone(s.GPUs)
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	var out []func(int) panel
	for _, g := range gpus {
		out = append(out, func(iw int) panel {
			return r.staled(panel{title: fmt.Sprintf("GPU %d (this host)", g.Index), tag: ascii(g.Name), tagRole: rDim,
				lines: r.gpuLines(g, iw)}, r.gpuStale())
		})
	}
	return out
}

// warnMark is the trailing marker that carries a warning without colour: "!"
// amber, "!!" red; "" when colour carries it.
func (r *renderer) warnMark(ro role) string {
	if !r.o.NoColor && !r.o.ASCII {
		return ""
	}
	switch ro {
	case rWarn:
		return "!"
	case rBad:
		return "!!"
	}
	return ""
}

func (r *renderer) gpuLines(g gpu.GPU, iw int) []string {
	const labelW, valW = 7, 13 // bars start in the same column as the Throughput gauges
	barW := min(iw-labelW-valW-2, maxGauge)
	na := r.paint(rDim, "n/a")
	row := func(label, val string, valRole role, frac float64, ro role, ok bool) string {
		l := fit(r.paint(rLabel, label), labelW)
		if !ok {
			return l + na
		}
		l += fit(r.paint(valRole, val+r.warnMark(valRole)), valW)
		if barW >= 4 {
			l += "  " + r.bar(frac, barW, ro, r.paint)
		}
		return l
	}
	memRole, memText := rMem, rValue
	memFrac, memHigh := vramFrac(g)
	if memHigh {
		memRole, memText = rWarn, rWarn
	}
	lines := []string{
		row("util", fmt.Sprintf("%.0f%%", g.UtilPct), rValue, g.UtilPct/100, rGood, g.HasUtil),
		row("vram", fmt.Sprintf("%.1f/%.1f GB", g.MemUsedMiB/1024, g.MemTotalMiB/1024), memText, memFrac, memRole, g.MemTotalMiB > 0),
	}
	temp := na
	if g.HasTemp {
		tr := tempRole(g.TempC, gpuTempWarn, gpuTempBad)
		temp = r.paint(tr, fmt.Sprintf("%.0f%sC", math.Round(g.TempC), r.g.deg)+r.warnMark(tr))
	}
	power := na
	if g.HasPower {
		// a card at its limit is normal use: its own figure is never a warning
		power = r.paint(rGood, fmt.Sprintf("%.0f W", g.PowerW))
	}
	l := fit(r.paint(rLabel, "temp"), labelW) + fit(temp, valW) + "  " + r.paint(rLabel, "power ") + power
	if g.HasFan {
		l += "   " + r.paint(rLabel, "fan ") + r.paint(rValue, fmt.Sprintf("%.0f%%", g.FanPct))
	}
	return append(lines, l)
}

// Energy panel detail levels: the totals block goes first when rows are short,
// then the cost line.
const (
	energyBasic = iota
	energyCost
	energyFull
)

// energyLines is the Energy panel content at the given detail level (the power
// history is a chart of the grid).
func (r *renderer) energyLines(iw, detail int) []string {
	s := r.s
	const labelW = 11
	label := func(l string) string { return fit(r.paint(rLabel, l), labelW) }
	pr := powerRole(s.PowerW, s.PSUWatts)
	draw := label("draw") + r.paint(pr, fmt.Sprintf("%.0f W", s.PowerW)+r.warnMark(pr))
	if s.PSUWatts > 0 { // the share of what the power supply can give (GPU + CPU package only)
		draw += r.paint(rLabel, fmt.Sprintf("  of %s W psu (%.0f%%)", axisNum(s.PSUWatts), s.PowerW/s.PSUWatts*100))
	}
	lines := []string{draw}
	if s.HasCPUPower && s.HasGPUPower {
		lines = append(lines, label("")+r.paint(rLabel, fmt.Sprintf("gpu %.0f W   cpu %.0f W", s.GPUPowerW, s.CPUPowerW)))
	}
	if !s.OneShot { // one-shot output covers about a second: no "since start"
		lines = append(lines, label("energy")+r.paint(rValue, fmtEnergy(s.EnergyWh))+r.paint(rLabel, " since start"))
	}
	eff := r.paint(rDim, "- (not generating)")
	if s.JoulesPerToken > 0 {
		eff = r.paint(rValue, fmt.Sprintf("%.1f J/token", s.JoulesPerToken)) + "   " +
			r.paint(rValue, fmt.Sprintf("%.0f tok/Wh", 3600/s.JoulesPerToken))
	}
	lines = append(lines, label("per token")+eff)
	if s.Cost.Enabled && detail >= energyCost {
		lines = append(lines, label("cost")+r.costText(iw-labelW))
	}
	if detail >= energyFull {
		lines = append(lines, r.totalsLines(iw, label)...)
	}
	return lines
}

// money formats an amount in the configured currency with glyphs the display has:
// ASCII spells the symbols out, the console font has the pound sign but no euro.
func (r *renderer) money(v float64) string {
	m := cost.Format(v, r.s.Cost.Currency)
	switch {
	case r.o.ASCII:
		m = strings.NewReplacer("£", "GBP ", "€", "EUR ").Replace(m)
	case r.o.Color16:
		m = strings.ReplaceAll(m, "€", "EUR ")
	}
	return safeMoney(m)
}

// safeMoney neutralises control and non-ASCII characters other than the symbols.
func safeMoney(s string) string {
	return strings.Map(func(c rune) rune {
		if c == '£' || c == '€' || (c >= 0x20 && c <= 0x7e) {
			return c
		}
		return '?'
	}, s)
}

// tariff is the price per kWh: the minor unit with two decimals for GBP, EUR and
// USD (30.31p/kWh, 30.31c/kWh), else Format(price)/kWh.
func tariff(c collect.Cost, money func(float64) string) string {
	if minor, ok := cost.Minor(c.Currency); ok {
		return fmt.Sprintf("%.2f%s/kWh", c.Price*100, minor)
	}
	return money(c.Price) + "/kWh"
}

// costText is the cost line in w cells: since start, per day at the average
// draw, per million generated tokens, and what it is based on. When narrow it
// sheds words before anything is cut: "at average draw", the power sources,
// the tariff, then the cost per million tokens.
func (r *renderer) costText(w int) string {
	s, p := r.s, r.paint
	c := s.Cost
	var src []string
	if s.HasGPUPower {
		src = append(src, "gpu")
	}
	if s.HasCPUPower {
		src = append(src, "cpu")
	}
	rate := tariff(c, r.money)
	// one-shot output (--frame) covers about a second: no "since start" figures, and
	// the day is priced at the current draw
	draw := " at average draw"
	if s.OneShot {
		draw = " at current draw"
	}
	build := func(drawWords, sources, basis, perM bool) string {
		day := p(rValue, r.money(c.PerDay)+"/day")
		if drawWords {
			day += p(rLabel, draw)
		}
		parts := []string{p(rValue, r.money(c.SinceStart)) + p(rLabel, " since start"), day}
		if s.OneShot {
			parts = parts[1:]
		}
		if perM && c.PerMTok > 0 && !s.OneShot {
			// an average since start, unlike the live J/token above it
			parts = append(parts, p(rValue, r.money(c.PerMTok))+p(rLabel, " per 1M tokens avg"))
		}
		if basis {
			b := rate
			if sources && len(src) > 0 {
				b = strings.Join(src, "+") + ", " + b
			}
			parts = append(parts, p(rDim, "("+b+")"))
		}
		return strings.Join(parts, "   ")
	}
	for _, v := range [][4]bool{{true, true, true, true}, {false, true, true, true}, {false, false, true, true},
		{false, false, false, true}} {
		if l := build(v[0], v[1], v[2], v[3]); width(l) <= w {
			return l
		}
	}
	return build(false, false, false, false)
}

// totalsLines are the today / month / all time rows from the database, columns
// aligned: energy, cost (when enabled), generated tokens, requests.
func (r *renderer) totalsLines(iw int, label func(string) string) []string {
	s, p := r.s, r.paint
	if !s.Today.HasData && !s.Month.HasData && !s.AllTime.HasData {
		return nil
	}
	rows := []struct {
		name string
		t    store.Totals
	}{{"today", s.Today}, {"month", s.Month}, {"all time", s.AllTime}}
	// cells first, so each column is as wide as its widest value
	var cells [3][4]string
	var w [4]int
	for i, row := range rows {
		u := row.t.Usage
		cells[i] = [4]string{fmtEnergy(u.GPUEnergyWh + u.CPUEnergyWh), r.money(u.Cost),
			fmtCount(int(u.GenTokens)), fmtCount(int(u.Requests))}
		for j, c := range cells[i] {
			if row.t.HasData {
				w[j] = max(w[j], width(c))
			}
		}
	}
	// short words when the long ones do not fit
	tok, req := " gen tokens", " requests"
	full := w[0] + 3 + w[2] + len(tok) + 3 + w[3] + len(req)
	if s.Cost.Enabled {
		full += 3 + w[1]
	}
	if width(label(""))+full > iw {
		tok, req = " gen", " req"
	}
	var out []string
	for i, row := range rows {
		if !row.t.HasData {
			out = append(out, label(row.name)+p(rDim, "no data"))
			continue
		}
		c := cells[i]
		l := p(rValue, padLeft(c[0], w[0]))
		if s.Cost.Enabled {
			l += "   " + p(rValue, padLeft(c[1], w[1]))
		}
		l += "   " + p(rValue, padLeft(c[2], w[2])) + p(rLabel, tok) +
			"   " + p(rValue, padLeft(c[3], w[3])) + p(rLabel, req)
		out = append(out, label(row.name)+l)
	}
	return out
}
