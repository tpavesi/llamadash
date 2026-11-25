package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"llamadash/internal/collect"
	"llamadash/internal/container"
)

// containersFixture is 5 running (the llama container among them, unhealthy by
// Docker but answering ok), 1 starting, 1 failed and 8 stopped containers.
func containersFixture() []container.Entry {
	run := func(n, health string) container.Entry {
		return container.Entry{Name: n, State: "running", Health: health}
	}
	exited := func(n string, code int) container.Entry {
		return container.Entry{Name: n, State: "exited", ExitCode: code, HasExit: true}
	}
	llama := run("llama-server-main", "unhealthy")
	llama.Llama = true
	return []container.Entry{
		run("web-1", "healthy"), llama, run("db-1", ""), run("proxy-1", "healthy"), run("cache-1", ""),
		run("search-1", "starting"),
		exited("tts-1", 128),
		exited("batch-1", 0), exited("old-1", 137), exited("old-2", 143),
		{Name: "fresh-1", State: "created"}, {Name: "pausy-1", State: "paused"}, {Name: "going-1", State: "removing"},
		{Name: "weird-1", State: "frobnicating"}, exited("unknown-1", 0),
	}
}

// ctLines is the inner lines (styled) of the full-width Containers panel in out.
func ctLines(out string, w int) []string {
	raw := lines(out)
	var res []string
	for i, l := range raw {
		if !strings.Contains(stripANSI(l), " Containers ") {
			continue
		}
		for _, c := range raw[i+1:] {
			if p := stripANSI(c); strings.HasPrefix(p, "└") || strings.HasPrefix(p, "+-") {
				break
			}
			res = append(res, ansi.Cut(c, 2, w-2))
		}
		break
	}
	return res
}

func withContainers(s collect.Snapshot) collect.Snapshot {
	s.Containers, s.ContainersAt = containersFixture(), s.At.Add(-4*time.Second)
	return s
}

func TestContainersPanel(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	o := opts(135, 120, s)
	raw := lines(Render(s, o))
	plain := lines(stripANSI(strings.Join(raw, "\n")))
	i := titleLine(plain, "┌ Containers ")
	if i < 0 {
		t.Fatalf("no panel:\n%s", strings.Join(plain, "\n"))
	}
	if !strings.Contains(plain[i], "Containers ─ 5 running, 1 starting, 1 failed, 8 stopped ") {
		t.Errorf("title %q", plain[i])
	}
	// directly after the llama-server panel
	if j := titleLine(plain, "┌ llama-server"); j < 0 || !strings.HasPrefix(plain[i-1], "└") || j+len(panelContent(raw, "llama-server"))+2 != i {
		t.Errorf("not directly after llama-server: %d %d", j, i)
	}
	cl := ctLines(strings.Join(raw, "\n"), 135)
	for i := range cl {
		cl[i] = strings.TrimSpace(stripANSI(cl[i]))
	}
	c := strings.Join(cl, "  ")
	want := "■ tts-1 (128)  ■ search-1  ■ cache-1  ■ db-1  ■ llama-server-main  ■ proxy-1  ■ web-1  ■ batch-1  ■ fresh-1  " +
		"■ going-1  ■ old-1  ■ old-2  ■ pausy-1  ■ unknown-1  ■ weird-1"
	if len(cl) != 2 || c != want {
		t.Errorf("content\n%q\nwant\n%q", c, want)
	}
	all := strings.Join(raw, "\n")
	r := newRenderer(s, o)
	for _, w := range []string{
		r.paint(rBad, "■") + " " + r.paint(rValue, "tts-1"),
		r.paint(rWarn, "■") + " " + r.paint(rValue, "search-1"),
		r.paint(rGood, "■") + " " + r.paint(rValue, "llama-server-main"),
		r.paint(rDim, "■") + " " + r.paint(rDim, "batch-1"),
	} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q", w)
		}
	}
	// zero counts other than running are left out
	s.Containers = []container.Entry{{Name: "a", State: "exited", ExitCode: 0, HasExit: true}}
	if p := lines(stripANSI(Render(s, o))); !strings.Contains(p[titleLine(p, "┌ Containers")], "Containers ─ 0 running, 1 stopped ") {
		t.Errorf("title %q", p[titleLine(p, "┌ Containers")])
	}
	for _, empty := range [][]container.Entry{nil, {}} {
		s.Containers = empty
		if strings.Contains(stripANSI(Render(s, o)), "Containers") {
			t.Error("panel without containers")
		}
	}
}

// The llama container is judged by what the server answers while it runs.
func TestContainersLlamaOverride(t *testing.T) {
	base := fullSnapshot(t)
	llama := container.Entry{Name: "llama", State: "running", Health: "unhealthy", Llama: true}
	cases := []struct {
		name string
		mut  func(*collect.Snapshot, *container.Entry)
		want container.Level
	}{
		{"answers ok", func(*collect.Snapshot, *container.Entry) {}, container.LevelOK},
		{"docker healthy, still loading", func(s *collect.Snapshot, e *container.Entry) { s.Loading, e.Health = true, "healthy" }, container.LevelStarting},
		{"unreachable", func(s *collect.Snapshot, e *container.Entry) { s.Reachable = false }, container.LevelStarting},
		{"health error", func(s *collect.Snapshot, e *container.Entry) { s.Health.Status = "error" }, container.LevelStarting},
		{"exited 128", func(s *collect.Snapshot, e *container.Entry) {
			s.Reachable, e.State, e.ExitCode, e.HasExit = false, "exited", 128, true
		}, container.LevelFailed},
		{"stopped", func(s *collect.Snapshot, e *container.Entry) { e.State, e.HasExit = "exited", true }, container.LevelOff},
		{"not the llama one", func(s *collect.Snapshot, e *container.Entry) { e.Llama = false }, container.LevelFailed},
	}
	for _, c := range cases {
		s, e := base, llama
		c.mut(&s, &e)
		if got := levelOf(e, s); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestContainersMarks(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	content := func(o Options) string {
		return strings.Join(ctLines(Render(s, o), o.Width), "\n")
	}
	o := opts(135, 120, s)
	o.NoColor = true
	if c := content(o); !strings.Contains(c, "! tts-1 (128)  ~ search-1  + cache-1") || !strings.Contains(c, "- batch-1") ||
		strings.Contains(c, "■") || strings.Contains(c, "\x1b") {
		t.Errorf("NO_COLOR marks:\n%s", c)
	}
	o.NoColor, o.ASCII = false, true
	r := newRenderer(s, o)
	if c := content(o); !strings.Contains(stripANSI(c), "! tts-1 (128)  ~ search-1  + cache-1") ||
		!strings.Contains(c, r.paint(rBad, "!")) || !strings.Contains(c, r.paint(rGood, "+")) {
		t.Errorf("ASCII marks:\n%q", c)
	}
	// TERM=linux: the plain 16-colour green, yellow and red of the palette's roles
	o.ASCII, o.Color16 = false, true
	c := content(o)
	for ro, sgr := range map[role]string{rGood: "92m", rWarn: "93m", rBad: "91m", rDim: "90m"} {
		if !strings.Contains(c, "\x1b["+sgr+"■") {
			t.Errorf("role %d: no %q mark in %q", ro, sgr, c)
		}
	}
}

func TestContainersOverflow(t *testing.T) {
	s := fullSnapshot(t)
	mk := func(n int, state string, code int) []container.Entry {
		var out []container.Entry
		for i := 0; i < n; i++ {
			out = append(out, container.Entry{Name: fmt.Sprintf("%s-%02d", state, i), State: state, ExitCode: code, HasExit: code > 0})
		}
		return out
	}
	o := opts(80, 120, s)
	// 12 green and "+30 stopped" fit in 3 rows of 76 cells; 30 stopped do not: they become "+30 stopped"
	s.Containers = append(mk(12, "running", 0), mk(30, "created", 0)...)
	c := stripANSI(strings.Join(ctLines(Render(s, o), o.Width), "\n"))
	if n := len(ctLines(Render(s, o), o.Width)); n > 3 || !strings.Contains(c, "+30 stopped") ||
		strings.Contains(c, "created-") || !strings.Contains(c, "running-11") {
		t.Errorf("%d rows:\n%s", n, c)
	}
	// 60 failed do not fit at all: the last item is "+N more" (counting the stopped)
	s.Containers = append(mk(60, "dead", 0), mk(4, "created", 0)...)
	pc := ctLines(Render(s, o), o.Width)
	c = stripANSI(strings.Join(pc, "\n"))
	if len(pc) != 3 || !strings.HasSuffix(strings.TrimSpace(stripANSI(pc[2])), "more") || strings.Contains(c, "stopped") {
		t.Fatalf("%d rows:\n%s", len(pc), c)
	}
	shown := strings.Count(c, "■ dead-")
	if !strings.Contains(c, fmt.Sprintf("+%d more", 64-shown)) {
		t.Errorf("%d shown:\n%s", shown, c)
	}
	// long names are cut to 36 cells
	s.Containers = []container.Entry{{Name: strings.Repeat("x", 40) + "\x1b[31m", State: "running"}}
	c = stripANSI(strings.Join(ctLines(Render(s, o), o.Width), "\n"))
	if !strings.Contains(c, "■ "+strings.Repeat("x", 36)+" ") || strings.Contains(c, strings.Repeat("x", 37)) {
		t.Errorf("long name:\n%s", c)
	}
}

func TestContainersStale(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	s.ContainersAt = s.At.Add(-25 * time.Second) // 3 x 10s: fresh
	raw := lines(Render(s, opts(135, 120, s)))
	plain := lines(stripANSI(strings.Join(raw, "\n")))
	if l := plain[titleLine(plain, "┌ Containers")]; strings.Contains(l, "stale") || dimmed(panelContent(raw, "Containers")) {
		t.Errorf("fresh tagged: %q", l)
	}
	s.ContainersAt = s.At.Add(-45 * time.Second)
	raw = lines(Render(s, opts(135, 120, s)))
	plain = lines(stripANSI(strings.Join(raw, "\n")))
	if l := plain[titleLine(plain, "┌ Containers")]; !strings.Contains(l, "stale, 45s ago") || !dimmed(panelContent(raw, "Containers")) {
		t.Errorf("stale: %q", l)
	}
}

func TestRenderPlainContainers(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	out := RenderPlain(s)
	assertPlain(t, out)
	if !strings.Contains(out, "\ncontainers  5 running, 1 starting, 1 failed, 8 stopped  failed: tts-1 (128)  starting: search-1\n") {
		t.Errorf("plain:\n%s", out)
	}
	s.Containers = containersFixture()[:5]
	if out := RenderPlain(s); !strings.Contains(out, "\ncontainers  5 running\n") {
		t.Errorf("plain, all running:\n%s", out)
	}
	s.Containers = nil
	if out := RenderPlain(s); strings.Contains(out, "containers") {
		t.Errorf("plain without a list:\n%s", out)
	}
}

// The header shows the model's quantisation after its name, and drops it first
// when the line is tight.
func TestHeaderQuantisation(t *testing.T) {
	s := fullSnapshot(t)
	p := *s.Props
	p.ModelAlias, p.ModelFtype = "demo-model", "Q4_K - Medium"
	s.Props = &p
	head := func(w int) string { return lines(stripANSI(Render(s, opts(w, 40, s))))[0] }
	if h := head(135); !strings.Contains(h, "demo-model Q4_K_M") {
		t.Errorf("135: %q", h)
	}
	if h := head(60); strings.Contains(h, "Q4_K_M") || !strings.Contains(h, "demo-model") || !strings.Contains(h, "ok") {
		t.Errorf("60: %q", h)
	}
	p.ModelFtype = "unknown, may not work"
	if h := head(135); strings.Contains(h, "unknown,") {
		t.Errorf("odd ftype shown: %q", h)
	}
	if out := RenderPlain(s); !strings.Contains(out, "model demo-model") {
		t.Errorf("plain: %q", strings.SplitN(out, "\n", 2)[0])
	}
	p.ModelFtype = "Q8_0"
	if out := RenderPlain(s); !strings.Contains(out, "model demo-model Q8_0") {
		t.Errorf("plain: %q", strings.SplitN(out, "\n", 2)[0])
	}
}
