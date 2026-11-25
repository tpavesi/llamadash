package ui

import (
	"time"

	"llamadash/internal/collect"
)

const (
	minChart   = 4 // chart body rows
	boxBorders = 2
)

// bodyAvail is the number of rows between the header (and status lines) and the footer.
func (r *renderer) bodyAvail() int {
	H := r.o.Height
	if H <= 1 {
		return 0
	}
	return max(H-2-len(r.status()), 0)
}

// bodyPlan is what the body shows in its rows.
type bodyPlan struct {
	reqH, recentN, detail int
	mid                   [][]string // the Throughput and GPU blocks that fit
	hostB, serverB, ctB   []string
	showEnergy            bool
	charts                [][]chartSlot // rows of the chart grid
	spacers               int           // rows nothing can use: single blank lines between blocks
}

// plan lays the panels out in avail rows: Requests, Recent requests, Throughput
// and GPU panels, Host, llama-server, Containers, Energy, then the chart grid. Every spare row
// goes to the charts, so the screen is filled; when no grid fits, spare rows go to
// more Recent requests rows, then to one full-width chart of the first metric (2
// body rows or more), and the rest (at most 3) become single blank lines between
// blocks, never a band. When rows run out the charts go
// first (from the end of their priority list), then Recent requests (down to
// recentMin rows, then the panel), Containers, llama-server, Host, the Energy totals and cost
// line; after that the Requests table shrinks, then the GPU and Energy panels go.
func (r *renderer) plan(avail int) (p bodyPlan) {
	W, s := r.o.Width, r.s
	if avail <= 0 {
		return p
	}
	landscape := W >= 100 && r.o.Height*2 <= W
	mid := r.midBlocks(landscape)
	midH := 0
	for _, b := range mid {
		midH += len(b)
	}
	// the Energy panel at each detail level
	var energyAt [energyFull + 1]int
	if s.HasPower {
		for d := range energyAt {
			energyAt[d] = len(r.energyLines(W-4, d)) + 2
		}
	}
	detail := energyFull
	energyH := energyAt[detail]
	reqNeed := r.requestsHeight(W-4) + 2

	// the version 1 panels first, then Host, llama-server, Containers and Recent
	// requests in what is left (in the reverse of their drop order)
	core := reqNeed + midH + energyH
	var hostB, serverB, ctB []string
	if s.Host != nil {
		hostB = r.box(r.hostPanel(W-4), W, 0)
	}
	if s.Proc != nil || s.Container != nil {
		serverB = r.box(r.serverPanel(), W, 0)
	}
	if len(s.Containers) > 0 {
		ctB = r.box(r.containersPanel(W-4), W, 0)
	}
	if core+len(hostB)+len(serverB)+len(ctB) > avail {
		ctB = nil
	}
	if core+len(hostB)+len(serverB) > avail {
		serverB = nil
	}
	if core+len(hostB) > avail {
		hostB = nil
	}
	for detail > energyBasic && core > avail { // then the Energy totals, then its cost line
		detail--
		core += energyAt[detail] - energyH
		energyH = energyAt[detail]
	}
	recentN := 0
	if n := min(len(s.Recent), recentMax); n > 0 {
		left := avail - core - len(hostB) - len(serverB) - len(ctB) - boxBorders - 1 // borders and column header
		if left >= min(n, recentMin) {
			recentN = min(n, left)
		}
	}
	avail -= len(hostB) + len(serverB) + len(ctB)
	if recentN > 0 {
		avail -= recentN + boxBorders + 1
	}

	reqH := reqNeed
	if reqH+midH+energyH > avail {
		reqH = max(min(reqNeed, 6), avail-midH-energyH)
	}
	reqH = min(reqH, avail)

	// decide which blocks fit, in order, before sharing out the spare rows
	used := reqH
	placed := 0
	for _, b := range mid {
		if used+len(b) > avail {
			break
		}
		used += len(b)
		placed++
	}
	showEnergy := s.HasPower && placed == len(mid) && used+energyH <= avail
	if showEnergy {
		used += energyH
	}
	spare := avail - used
	p = bodyPlan{reqH: reqH, recentN: recentN, detail: detail, mid: mid[:placed], hostB: hostB, serverB: serverB,
		ctB: ctB, showEnergy: showEnergy}
	specs := r.chartSpecs()
	if placed == len(mid) {
		minBody := minChart
		if r.o.Range.long() {
			minBody++ // the time axis row
		}
		if p.charts = gridPlan(specs, spare, W, minBody); len(p.charts) > 0 {
			return p
		}
	}
	if p.recentN > 0 { // more Recent requests rows, beyond the usual recentMax
		add := min(spare, len(s.Recent)-p.recentN)
		p.recentN += add
		spare -= add
	}
	if spare >= boxBorders+2 { // a filler chart, below the grid's minimum height
		p.charts = [][]chartSlot{{{specs[0], W, spare - boxBorders}}}
		spare = 0
	}
	p.spacers = spare
	return p
}

// body draws the plan for avail rows.
func (r *renderer) body(avail int) []string {
	W := r.o.Width
	p := r.plan(avail)
	if avail <= 0 {
		return nil
	}
	blocks := [][]string{r.box(r.staled(panel{title: "Requests", lines: r.requestsLines(W-4, p.reqH-2)}, r.serverStale()), W, p.reqH)}
	if p.recentN > 0 {
		blocks = append(blocks, r.box(r.recentPanel(W-4, p.recentN), W, 0))
	}
	blocks = append(blocks, p.mid...)
	for _, b := range [][]string{p.hostB, p.serverB, p.ctB} {
		if len(b) > 0 {
			blocks = append(blocks, b)
		}
	}
	if p.showEnergy {
		blocks = append(blocks, r.box(r.staled(panel{title: "Energy", lines: r.energyLines(W-4, p.detail)}, r.energyStale()), W, 0))
	}
	for _, row := range p.charts {
		cols := make([][]string, len(row))
		for i, c := range row {
			cols[i] = r.box(r.chartPanel(c.spec, c.w-4, c.h), c.w, c.h+boxBorders)
		}
		blocks = append(blocks, joinH(cols))
	}
	// rows nothing can use: one blank line after each block, from the bottom up
	gapAfter := make([]bool, len(blocks))
	for i := len(blocks) - 1; i >= 0 && p.spacers > 0; i-- {
		gapAfter[i] = true
		p.spacers--
	}
	var out []string
	for i, b := range blocks {
		out = append(out, b...)
		if gapAfter[i] {
			out = append(out, "")
		}
	}
	return out
}

// chartSlot is one chart of the grid: its box width and body rows.
type chartSlot struct {
	spec chartSpec
	w, h int
}

// gridPlan places the charts in spare rows of a screen W wide: two per row when W
// >= 120, else one. Each chart needs minBody body rows; when there is not room
// for all, charts are dropped from the end of the list. The spare rows are shared
// equally between the chart rows, the remainder one each to the first rows. A
// lone chart in the last row takes the full width.
func gridPlan(specs []chartSpec, spare, W, minBody int) [][]chartSlot {
	cols := 1
	if W >= 120 {
		cols = 2
	}
	rowsFor := func(k int) int { return (k + cols - 1) / cols }
	k := len(specs)
	for k > 0 && rowsFor(k)*(minBody+boxBorders) > spare {
		k--
	}
	if k == 0 {
		return nil
	}
	n := rowsFor(k)
	body := spare - n*boxBorders
	each, extra := body/n, body%n
	out := make([][]chartSlot, n)
	for i := range out {
		h := each
		if i < extra {
			h++
		}
		row := specs[i*cols : min((i+1)*cols, k)]
		if len(row) == 1 {
			out[i] = []chartSlot{{row[0], W, h}}
			continue
		}
		out[i] = []chartSlot{{row[0], W / 2, h}, {row[1], W - W/2, h}}
	}
	return out
}

// chartPlotWidths is the plot width of each chart s shows at o, by chart key: the
// bucket count of its long-range query.
func chartPlotWidths(s collect.Snapshot, o Options) map[string]int {
	out := map[string]int{}
	if o.Width <= 0 || o.Height <= 0 {
		return out
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	r := newRenderer(s, o)
	for _, row := range r.plan(r.bodyAvail()).charts {
		for _, c := range row {
			if _, plot := longGeom(c.w - 4); plot > 0 {
				out[c.spec.key] = plot
			}
		}
	}
	return out
}

// midBlocks renders the Throughput and GPU panels: stacked, or side by side in
// landscape (all in one row when each gets 40 columns, else GPUs stacked on the right).
func (r *renderer) midBlocks(landscape bool) [][]string {
	W := r.o.Width
	tp := r.throughputPanel
	gpus := r.gpuPanels()
	if !landscape || len(gpus) == 0 {
		blocks := [][]string{r.box(tp(W-4), W, 0)}
		for _, g := range gpus {
			blocks = append(blocks, r.box(g(W-4), W, 0))
		}
		return blocks
	}
	n := 1 + len(gpus)
	if W/n >= 40 {
		ws := make([]int, n)
		ps := make([]panel, n)
		h := 0
		for i := range ws {
			ws[i] = W / n
			if i == n-1 {
				ws[i] = W - (n-1)*(W/n)
			}
			if i == 0 {
				ps[i] = tp(ws[i] - 4)
			} else {
				ps[i] = gpus[i-1](ws[i] - 4)
			}
			h = max(h, len(ps[i].lines)+2)
		}
		cols := make([][]string, n)
		for i := range ps {
			cols[i] = r.box(ps[i], ws[i], h)
		}
		return [][]string{joinH(cols)}
	}
	lw := W / 2
	rw := W - lw
	var right [][]string
	rh := 0
	for _, g := range gpus {
		b := r.box(g(rw-4), rw, 0)
		right = append(right, b)
		rh += len(b)
	}
	lp := tp(lw - 4)
	h := max(rh, len(lp.lines)+2)
	if last := len(right) - 1; rh < h {
		right[last] = r.box(gpus[last](rw-4), rw, len(right[last])+h-rh)
	}
	var rcol []string
	for _, b := range right {
		rcol = append(rcol, b...)
	}
	return [][]string{joinH([][]string{r.box(lp, lw, h), rcol})}
}

// joinH places equal-height blocks side by side.
func joinH(cols [][]string) []string {
	h := 0
	for _, c := range cols {
		h = max(h, len(c))
	}
	out := make([]string, h)
	for _, c := range cols {
		w := 0
		for _, l := range c {
			w = max(w, width(l))
		}
		for i := range out {
			l := ""
			if i < len(c) {
				l = c[i]
			}
			out[i] += fit(l, w)
		}
	}
	return out
}
