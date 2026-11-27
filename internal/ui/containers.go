package ui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"llamadash/internal/collect"
	"llamadash/internal/container"
)

const (
	ctRows    = 3  // rows of the Containers panel at most
	ctNameMax = 36 // cells of a container name
)

// levelOf is the light of e. The container publishing the dashboard's port is,
// while it runs, judged by what the server answers rather than by its Docker
// healthcheck: ok when it answers ok, else still booting.
func levelOf(e container.Entry, s collect.Snapshot) container.Level {
	if e.Llama && e.State == "running" {
		if s.Reachable && !s.Loading && s.Health.Status == "ok" {
			return container.LevelOK
		}
		return container.LevelStarting
	}
	return e.Level()
}

// ctItem is one container with its light.
type ctItem struct {
	e     container.Entry
	level container.Level
}

// ctRank orders the levels for display: failed, starting, ok, off.
func ctRank(l container.Level) int {
	switch l {
	case container.LevelFailed:
		return 0
	case container.LevelStarting:
		return 1
	case container.LevelOK:
		return 2
	}
	return 3
}

// ctItems is s.Containers with their lights, failed first, by name within a level.
func ctItems(s collect.Snapshot) []ctItem {
	items := make([]ctItem, len(s.Containers))
	for i, e := range s.Containers {
		items[i] = ctItem{e, levelOf(e, s)}
	}
	slices.SortStableFunc(items, func(a, b ctItem) int {
		return cmp.Or(cmp.Compare(ctRank(a.level), ctRank(b.level)), strings.Compare(a.e.Name, b.e.Name))
	})
	return items
}

// ctCounts is the number of containers at each level.
func ctCounts(items []ctItem) (n [4]int) {
	for _, it := range items {
		n[it.level]++
	}
	return n
}

// ctSummary is "5 running, 1 starting, 1 failed, 8 stopped", zero counts left out
// except running.
func ctSummary(items []ctItem) string {
	n := ctCounts(items)
	parts := []string{fmt.Sprintf("%d running", n[container.LevelOK])}
	for _, c := range []struct {
		l    container.Level
		name string
	}{{container.LevelStarting, "starting"}, {container.LevelFailed, "failed"}, {container.LevelOff, "stopped"}} {
		if n[c.l] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[c.l], c.name))
		}
	}
	return strings.Join(parts, ", ")
}

// ctMark is the mark of a level: a coloured ■, or without colour (and in ASCII
// mode) a character that tells the levels apart on its own.
func (r *renderer) ctMark(l container.Level) string {
	mark, ro := "-", ctLevelRole(l)
	switch l {
	case container.LevelOK:
		mark = "+"
	case container.LevelStarting:
		mark = "~"
	case container.LevelFailed:
		mark = "!"
	}
	if !r.o.NoColor && !r.o.ASCII {
		mark = "■"
	}
	return r.paint(ro, mark)
}

// ctLabel is one item: mark, name (cut to ctNameMax) and a failed container's exit code.
func (r *renderer) ctLabel(it ctItem) string {
	name, ro := ansi.Truncate(ascii(it.e.Name), ctNameMax, ""), rValue
	if it.level == container.LevelOff {
		ro = rDim
	}
	l := r.ctMark(it.level) + " " + r.paint(ro, name)
	if it.level == container.LevelFailed && it.e.HasExit {
		l += " " + r.paint(rLabel, fmt.Sprintf("(%d)", it.e.ExitCode))
	}
	return l
}

// flow places items left to right, two spaces apart, wrapping at w cells; ok is
// false when they need more than ctRows rows. An item wider than w has a row of
// its own (the box cuts it).
func flow(items []string, w int) (rows []string, ok bool) {
	line := ""
	for _, it := range items {
		switch {
		case line == "":
			line = it
		case width(line)+2+width(it) <= w:
			line += "  " + it
		default:
			rows = append(rows, line)
			line = it
		}
	}
	if line != "" {
		rows = append(rows, line)
	}
	return rows, len(rows) <= ctRows
}

// containersPanel shows every container as a mark and its name, in up to ctRows
// rows of iw cells. When they do not all fit the stopped ones go first, replaced
// by "+N stopped"; if still too many, the last item is "+N more".
func (r *renderer) containersPanel(iw int) panel {
	items := ctItems(r.s)
	labels := make([]string, len(items))
	alarm := 0 // items not off: the off ones are last
	for i, it := range items {
		labels[i] = r.ctLabel(it)
		if it.level != container.LevelOff {
			alarm = i + 1
		}
	}
	rows, ok := flow(labels, iw)
	if !ok {
		more := func(n int, what string) string { return r.paint(rDim, fmt.Sprintf("+%d %s", n, what)) }
		if off := len(items) - alarm; off > 0 {
			rows, ok = flow(append(slices.Clone(labels[:alarm]), more(off, "stopped")), iw)
		}
		for k := alarm; !ok && k >= 0; k-- {
			rows, ok = flow(append(slices.Clone(labels[:k]), more(len(items)-k, "more")), iw)
		}
	}
	if len(rows) > ctRows {
		rows = rows[:ctRows]
	}
	p := panel{title: "Containers", tag: ctSummary(items), tagRole: rLabel, lines: rows}
	return r.staled(p, r.containersStale())
}

// plainContainers is the --once line: the counts, then the failed and starting ones.
func plainContainers(line func(string, ...any), s collect.Snapshot) {
	if len(s.Containers) == 0 {
		return
	}
	items := ctItems(s)
	l := "containers  " + ctSummary(items)
	var failed, starting []string
	for _, it := range items {
		name := ascii(it.e.Name)
		switch it.level {
		case container.LevelFailed:
			if it.e.HasExit {
				name += fmt.Sprintf(" (%d)", it.e.ExitCode)
			}
			failed = append(failed, name)
		case container.LevelStarting:
			starting = append(starting, name)
		}
	}
	if len(failed) > 0 {
		l += "  failed: " + strings.Join(failed, ", ")
	}
	if len(starting) > 0 {
		l += "  starting: " + strings.Join(starting, ", ")
	}
	line("%s", l)
}
