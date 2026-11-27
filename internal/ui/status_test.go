package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"llamadash/internal/collect"
	"llamadash/internal/container"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/llama"
	"llamadash/internal/stats"
)

func TestPowerRole(t *testing.T) {
	for _, c := range []struct {
		w, psu float64
		want   role
	}{
		{500, 0, rValue},     // no power supply wattage: neutral, no tier
		{790, 1000, rGood},   // 79%
		{794.9, 1000, rGood}, // shows as 79%
		{795, 1000, rWarn},   // shows as 80%: the tier follows the figure shown
		{944, 1000, rWarn},   // 94%
		{949, 1000, rBad},    // shows as 95%
		{950, 1000, rBad},
		{1200, 1000, rBad},
	} {
		if got := powerRole(c.w, c.psu); got != c.want {
			t.Errorf("powerRole(%v, %v) = %v, want %v", c.w, c.psu, got, c.want)
		}
	}
}

func TestEnergyDrawTier(t *testing.T) {
	s := idleSnapshot()
	s.HasPower, s.PSUWatts = true, 1000
	for w, want := range map[float64]string{960: "960 W!!  of 1000 W psu (96%)", 850: "850 W!  of 1000 W psu (85%)", 300: "300 W  of 1000 W psu (30%)"} {
		s.PowerW = w
		o := opts(120, 40, s)
		o.NoColor = true
		if out := Render(s, o); !strings.Contains(out, want) {
			t.Errorf("draw %v W: want %q in\n%s", w, want, out)
		}
	}
}

func TestGPUPowerIsGreen(t *testing.T) {
	s := idleSnapshot()
	r := newRenderer(s, opts(120, 40, s))
	g := gpu.GPU{Name: "Example GPU", PowerW: 250, HasPower: true}
	if got := strings.Join(r.gpuLines(g, 60), "\n"); !strings.Contains(got, r.paint(rGood, "250 W")) {
		t.Errorf("GPU power is not painted green:\n%q", got)
	}
}

// paintedLevel is the worst warning colour in a 256-colour frame.
func paintedLevel(out string) Level {
	switch {
	case strings.Contains(out, "38;5;203"):
		return LevelBad
	case strings.Contains(out, "38;5;221"):
		return LevelWarn
	}
	return LevelOK
}

// healthy is a snapshot with every source present and nothing amber or red.
func healthy() collect.Snapshot {
	s := idleSnapshot()
	s.GPUs, s.GPUAt = []gpu.GPU{{Name: "Example GPU", UtilPct: 40, HasUtil: true, MemUsedMiB: 8000, MemTotalMiB: 24000,
		TempC: 50, HasTemp: true, PowerW: 200, HasPower: true}}, s.At
	s.Host, s.HostAt = &host.Info{CPUPct: 10, HasCPU: true, Cores: 8, MemTotalKB: 64 << 20, MemAvailKB: 48 << 20}, s.At
	s.Fans, s.HwmonAt = []collect.Fan{{Label: "fan1", RPM: 900}, {Label: "fan2", RPM: 1100}}, s.At
	s.CPUTempC, s.HasCPUTemp = 45, true
	s.Container, s.ContainerAt = &container.Info{Name: "server", State: "running", Health: "healthy"}, s.At
	s.Containers, s.ContainersAt = []container.Entry{{Name: "server", State: "running", Health: "healthy"}}, s.At
	s.HasPower, s.HasGPUPower, s.PowerW, s.GPUPowerW, s.PSUWatts = true, true, 300, 300, 1000
	return s
}

func statusOpts(s collect.Snapshot) Options {
	return Options{Width: 220, Height: 140, Now: s.At, Interval: time.Second}
}

func TestStatusMatchesScreen(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*collect.Snapshot)
		want Level
	}{
		{"base", func(*collect.Snapshot) {}, LevelOK},
		{"connecting", func(s *collect.Snapshot) { *s = collect.Snapshot{} }, LevelWarn},
		{"unreachable", func(s *collect.Snapshot) { s.Reachable = false }, LevelBad},
		{"loading", func(s *collect.Snapshot) { s.Loading = true }, LevelWarn},
		{"health", func(s *collect.Snapshot) { s.Health = llama.Health{Status: "degraded"} }, LevelWarn},
		{"gpu warm", func(s *collect.Snapshot) { s.GPUs[0].TempC = 79.6 }, LevelWarn},
		{"gpu hot", func(s *collect.Snapshot) { s.GPUs[0].TempC = 90 }, LevelBad},
		{"vram", func(s *collect.Snapshot) { s.GPUs[0].MemUsedMiB = 22800 }, LevelWarn},
		{"cpu warm", func(s *collect.Snapshot) { s.CPUTempC = 85 }, LevelWarn},
		{"cpu hot", func(s *collect.Snapshot) { s.CPUTempC = 95 }, LevelBad},
		{"ram", func(s *collect.Snapshot) { s.Host.MemAvailKB = s.Host.MemTotalKB / 10 }, LevelWarn},
		{"fan stopped", func(s *collect.Snapshot) { s.Fans[1] = collect.Fan{Label: "fan2", Stopped: true} }, LevelBad},
		{"container starting", func(s *collect.Snapshot) { s.Container.Health = "starting" }, LevelWarn},
		{"container unhealthy", func(s *collect.Snapshot) { s.Container.Health = "unhealthy" }, LevelBad},
		{"container exited", func(s *collect.Snapshot) { s.Container.State = "exited" }, LevelBad},
		{"list starting", func(s *collect.Snapshot) {
			s.Containers = append(s.Containers, container.Entry{Name: "b", State: "restarting"})
		}, LevelWarn},
		{"list failed", func(s *collect.Snapshot) {
			s.Containers = append(s.Containers, container.Entry{Name: "b", State: "exited", ExitCode: 1, HasExit: true})
		}, LevelBad},
		{"list stopped", func(s *collect.Snapshot) {
			s.Containers = append(s.Containers, container.Entry{Name: "b", State: "exited", HasExit: true})
		}, LevelOK},
		{"db", func(s *collect.Snapshot) { s.DBStatus = "off (disk full)" }, LevelWarn},
		{"slots hint", func(s *collect.Snapshot) { s.SlotsHint = "slots endpoint disabled" }, LevelWarn},
		{"stale gpu", func(s *collect.Snapshot) { s.GPUs[0].TempC, s.GPUAt = 95, s.At.Add(-10*time.Second) }, LevelWarn},
		{"stale server", func(s *collect.Snapshot) { s.SlotsAt = s.At.Add(-10 * time.Second) }, LevelWarn},
		{"stale host", func(s *collect.Snapshot) { s.CPUTempC, s.HwmonAt = 99, s.At.Add(-10*time.Second) }, LevelWarn},
		{"stale container", func(s *collect.Snapshot) {
			s.Container.State, s.ContainerAt = "exited", s.At.Add(-time.Minute)
		}, LevelWarn},
		{"stale list", func(s *collect.Snapshot) {
			s.Containers[0].Health, s.ContainersAt = "unhealthy", s.At.Add(-time.Minute)
		}, LevelWarn},
		{"draw high", func(s *collect.Snapshot) { s.PowerW = 850 }, LevelWarn},
		{"draw over", func(s *collect.Snapshot) { s.PowerW = 960 }, LevelBad},
		{"no psu", func(s *collect.Snapshot) { s.PowerW, s.PSUWatts = 960, 0 }, LevelOK},
	} {
		s := healthy()
		c.edit(&s)
		o := statusOpts(s)
		if o.Now.IsZero() {
			o.Now = t0
		}
		got, shown := Status(s, o), paintedLevel(Render(s, o))
		if got != c.want || shown != c.want {
			t.Errorf("%s: Status = %v, screen = %v, want %v", c.name, got, shown, c.want)
		}
	}
}

// The status does not depend on the size of the terminal or the display mode.
func TestStatusIgnoresDisplay(t *testing.T) {
	s := healthy()
	s.Fans[0].Stopped = true
	for _, o := range []Options{{}, {Width: 20, Height: 3}, {NoColor: true, ASCII: true, Paused: true}} {
		o.Now = s.At
		if got := Status(s, o); got != LevelBad {
			t.Errorf("Status with %+v = %v, want bad", o, got)
		}
	}
}

func TestStatusExceptions(t *testing.T) {
	slot := func(st stats.State, used int) func(*collect.Snapshot, *Options) {
		return func(s *collect.Snapshot, _ *Options) {
			s.Slots = []stats.SlotView{{Slot: llama.Slot{ID: 0, NCtx: 1000}, State: st, CtxUsed: used}}
		}
	}
	for name, edit := range map[string]func(*collect.Snapshot, *Options){
		"missing fan":    func(s *collect.Snapshot, _ *Options) { s.Fans[1] = collect.Fan{Label: "fan2", Missing: true} },
		"generating 85%": slot(stats.Generating, 850),
		"generating 97%": slot(stats.Generating, 970),
		"prompt 85%":     slot(stats.PromptEval, 850),
		"prompt 97%":     slot(stats.PromptEval, 970),
		"history unavailable": func(_ *collect.Snapshot, o *Options) {
			o.LongErr, o.Range, o.History = true, Range1h, true
		},
	} {
		s := healthy()
		o := statusOpts(s)
		edit(&s, &o)
		if shown := paintedLevel(Render(s, o)); shown == LevelOK {
			t.Errorf("%s: the screen shows no warning colour", name)
		}
		if got := Status(s, o); got != LevelOK {
			t.Errorf("%s: Status = %v, want ok", name, got)
		}
	}
}

func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{LevelOK: "ok", LevelWarn: "warn", LevelBad: "bad", Level(9): "bad", Level(-1): "ok"} {
		if got := l.String(); got != want {
			t.Errorf("Level(%d) = %q, want %q", int(l), got, want)
		}
	}
}

func statusModel(t *testing.T, o Options) *Model {
	m := NewModel(collect.New(collect.Config{URL: "http://127.0.0.1:1", GPU: collect.GPUOff}), time.Second, o, nil).(*Model)
	m.now = func() time.Time { return t0 }
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

func TestModelReportsStatus(t *testing.T) {
	var got []Level
	m := statusModel(t, Options{OnStatus: func(l Level) string {
		got = append(got, l)
		return "led: unavailable"
	}})
	if v := m.View().Content; strings.Contains(v, "led:") {
		t.Fatalf("a note before any poll:\n%s", stripANSI(v))
	}
	m.Update(pollMsg{snap: unreachableSnapshot()})
	if len(got) != 1 || got[0] != LevelBad {
		t.Fatalf("OnStatus got %v, want one bad", got)
	}
	if v := lastLine(m.View().Content); !strings.Contains(v, "led: unavailable") {
		t.Errorf("footer = %q", v)
	}
	m.Update(pollMsg{snap: idleSnapshot()})
	if len(got) != 2 || got[1] != LevelOK {
		t.Errorf("OnStatus got %v, want bad then ok", got)
	}
}

// Pause freezes the picture, not the lights: they follow live data, and the
// stale tags are judged by the live clock.
func TestModelReportsStatusWhilePaused(t *testing.T) {
	var got []Level
	note := "led: unavailable"
	m := statusModel(t, Options{OnStatus: func(l Level) string {
		got = append(got, l)
		return note
	}})
	m.Update(pollMsg{snap: idleSnapshot()})
	m.Update(key("p"))
	m.now = func() time.Time { return t0.Add(time.Hour) } // the frozen clock stays at t0
	note = ""
	m.Update(pollMsg{snap: unreachableSnapshot()})
	s := idleSnapshot()
	s.At = t0.Add(time.Hour) // fresh at the live clock, an hour old at the frozen one
	s.SlotsAt = s.At
	m.Update(pollMsg{snap: s})
	if want := []Level{LevelOK, LevelBad, LevelOK}; !slices.Equal(got, want) {
		t.Errorf("OnStatus got %v, want %v", got, want)
	}
	if v := lastLine(m.View().Content); strings.Contains(v, "led:") {
		t.Errorf("the note did not clear while paused: %q", v)
	}
}

func TestModelWithoutOnStatus(t *testing.T) {
	m := statusModel(t, Options{})
	m.Update(pollMsg{snap: unreachableSnapshot()})
	if v := m.View().Content; strings.Contains(v, "led:") {
		t.Errorf("a note without OnStatus:\n%s", stripANSI(v))
	}
}

func TestFooterNote(t *testing.T) {
	s := idleSnapshot()
	o := opts(120, 40, s)
	o.LEDNote = "led: unavailable"
	foot := func(o Options) string { ls := lines(Render(s, o)); return ls[len(ls)-1] }
	if f := foot(o); !strings.Contains(f, newRenderer(s, o).paint(rWarn, "led: unavailable")) || !strings.Contains(f, "localhost:8080") {
		t.Errorf("footer = %q", f)
	}
	o.NoColor = true
	if f := foot(o); !strings.Contains(f, "led: unavailable!   ") || !strings.HasSuffix(f, "http://localhost:8080") {
		t.Errorf("no colour: footer = %q", f)
	}
	// the URL gives way first, then the note; the keys always stay
	o.Width = 70
	if f := foot(o); !strings.Contains(f, "led: unavailable!") || strings.Contains(f, "localhost") || !strings.HasPrefix(f, "q quit") {
		t.Errorf("width 70: footer = %q", f)
	}
	o.Width = 40
	if f := foot(o); strings.Contains(f, "led") || !strings.HasPrefix(f, "q quit   p pause") || width(f) != 40 {
		t.Errorf("width 40: footer = %q (%d cells)", f, width(f))
	}
	// an outside string: control characters go, and it is cut at 32 cells
	o.Width, o.LEDNote = 120, "led: \x1b[31m"+strings.Repeat("x", 80)
	if f := foot(o); strings.Contains(f, "\x1b") || strings.Contains(f, strings.Repeat("x", 28)) || !strings.Contains(f, "led: ?[31m"+strings.Repeat("x", 22)+"!") {
		t.Errorf("long note: footer = %q", f)
	}
	// the note is not part of the status
	if got := Status(healthy(), Options{LEDNote: "led: unavailable", Now: healthy().At}); got != LevelOK {
		t.Errorf("Status with a note = %v", got)
	}
}

func TestFooterNoteFits(t *testing.T) {
	s := busy(t, 60)
	for _, mode := range []Options{{}, {Color16: true}, {NoColor: true}, {ASCII: true}} {
		for w := 1; w <= 200; w += 3 {
			for _, h := range []int{1, 2, 10, 40} {
				o := mode
				o.Width, o.Height, o.Interval, o.Now, o.LEDNote, o.History = w, h, time.Second, s.At, "led: unavailable", w%2 == 0
				checkFits(t, "note", Render(s, o), o)
			}
		}
	}
}
