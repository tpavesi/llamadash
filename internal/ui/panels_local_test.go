package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/container"
	"llamadash/internal/gpu"
	"llamadash/internal/host"
	"llamadash/internal/llama"
	"llamadash/internal/procstat"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

// fullAt is the realistic snapshot's poll time: 3h12m after the container started.
var (
	fullStarted = time.Date(2026, 11, 24, 22, 36, 47, 0, time.UTC)
	fullAt      = fullStarted.Add(3*time.Hour + 12*time.Minute + 3*time.Second)
)

// hostFixture is the Task 2 /proc fixture as host.Info (RAM 18.6/251.5 GB, swap
// 0.0/1.0 GB, load 1.14 1.15 1.10) with a disk of 1.5T free of 1.8T.
func hostFixture() *host.Info {
	return &host.Info{CPUPct: 3.2, HasCPU: true, Cores: 64, Load1: 1.14, Load5: 1.15, Load15: 1.10,
		MemTotalKB: 263764736, MemAvailKB: 244305928, SwapTotalKB: 1000444, SwapFreeKB: 1000444,
		DiskTotal: 1843 << 30, DiskFree: 1536 << 30, HasDisk: true}
}

// fansFixture is the AMD board of testdata/hwmon/amd as the collector shows it:
// the four connected headers of the nct6798, the three that read 0 left out.
func fansFixture() []collect.Fan {
	f := func(l string, rpm int) collect.Fan { return collect.Fan{Label: l, Chip: "nct6798", RPM: rpm} }
	return []collect.Fan{f("fan 1", 1250), f("fan 2", 890), f("fan 5", 1020), f("fan 7", 1460)}
}

func procFixture() *procstat.Info {
	return &procstat.Info{PID: 115429, CPUPct: 105, HasCPU: true, RSSKB: 12263340, Threads: 103}
}

func containerFixture() *container.Info {
	return &container.Info{Name: "llama-server-main", Health: "unhealthy", State: "running", StartedAt: fullStarted}
}

func rtx5090() gpu.GPU {
	return gpu.GPU{Index: 0, Name: "NVIDIA GeForce RTX 5090", UtilPct: 93, MemUsedMiB: 29965, MemTotalMiB: 32607,
		TempC: 64.37, PowerW: 460.46, HasUtil: true, HasTemp: true, HasPower: true, FanPct: 33, HasFan: true}
}

// recentFixture returns n finished requests, newest first, the newest at `at`;
// request 1 (the second newest) was already running at first sight.
func recentFixture(n int, at time.Time) []stats.Finished {
	out := make([]stats.Finished, n)
	for i := range out {
		gen := 300 + i*137
		out[i] = stats.Finished{At: at.Add(-time.Duration(i) * 95 * time.Second), SlotID: 0, TaskID: 1000 - i,
			PromptTokens: 131727 + i*2111, CacheShare: 0.97 - float64(i)*0.01, Generated: gen,
			Duration: time.Duration(gen)*12*time.Millisecond + 800*time.Millisecond, TTFT: 800 * time.Millisecond,
			GenRate: 83.4 - float64(i)*0.6}
	}
	if n > 1 {
		out[1].Approx = true
	}
	if n > 2 {
		out[2].GenRate, out[2].TTFT = 0, 0
	}
	return out
}

// fullSnapshot is a realistic snapshot of the live server with every panel populated.
func fullSnapshot(t *testing.T) collect.Snapshot {
	s := busy(t, 120)
	s.At, s.SlotsAt, s.URL = fullAt, fullAt, "http://127.0.0.1:8011"
	s.GenHist, s.PromptHist, s.PowerHist = realistic()
	s.GenRate, s.PromptRate = s.GenHist[119], 0
	s.Slots[0].GenRate = s.GenRate
	s.MetricsHint = ""
	s.Metrics = llama.Metrics{"requests_deferred": 0, "requests_processing": 1,
		"tokens_predicted_total": 1.2e6, "prompt_tokens_total": 8.4e6}
	s.GPUs, s.GPUAt = []gpu.GPU{rtx5090()}, fullAt
	s.HasGPUPower, s.GPUPowerW, s.HasCPUPower, s.CPUPowerW = true, 460.46, true, 118
	s.PowerW = s.GPUPowerW + s.CPUPowerW
	s.EnergyWh, s.GPUEnergyWh, s.CPUEnergyWh = 1530, 1210, 320
	s.JoulesPerToken = s.PowerW / s.GenRate
	s.Host, s.HostAt = hostFixture(), fullAt
	s.Fans, s.CPUTempC, s.HasCPUTemp, s.HwmonAt = fansFixture(), 60.125, true, fullAt
	s.HostCPUHist = hist(120)
	s.GPUUtilHist, s.GPUMemHist, s.GPUTempHist = gpuHists()
	// 120 polls 1 s apart, 60 GPU results 2 s apart: two minutes each
	s.GenHistFrom, s.PowerHistFrom = fullAt.Add(-2*time.Minute), fullAt.Add(-2*time.Minute)
	s.GPUHistFrom, s.HostCPUHistFrom = fullAt.Add(-2*time.Minute), fullAt.Add(-2*time.Minute)
	s.Proc, s.ProcAt = procFixture(), fullAt
	s.Container, s.ContainerAt = containerFixture(), fullAt.Add(-4*time.Second)
	s.Recent = recentFixture(12, fullAt.Add(-20*time.Second))
	s.Cost = collect.Cost{Enabled: true, Currency: "GBP", Price: 0.3031, SinceStart: 1.530 * 0.3031,
		PerDay: 0.478 * 24 * 0.3031, PerMTok: 1.530 * 0.3031 / 0.98}
	s.GenTokensSinceStart = 980_000
	s.Today = store.Totals{HasData: true, Usage: store.Usage{GenTokens: 1_210_000, PromptTokens: 9_800_000,
		Requests: 42, GPUEnergyWh: 9100, CPUEnergyWh: 2400, Cost: 11.5 * 0.3031}}
	s.Month = store.Totals{HasData: true, Usage: store.Usage{GenTokens: 31_400_000, PromptTokens: 251_000_000,
		Requests: 1093, GPUEnergyWh: 241_000, CPUEnergyWh: 63_000, Cost: 304 * 0.3031}}
	s.AllTime = store.Totals{HasData: true, Usage: store.Usage{GenTokens: 88_700_000, PromptTokens: 702_000_000,
		Requests: 3120, GPUEnergyWh: 690_000, CPUEnergyWh: 181_000, Cost: 871 * 0.3031}}
	return s
}

func TestRenderRecent(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	raw := lines(Render(s, o))
	plain := lines(stripANSI(strings.Join(raw, "\n")))
	c := panelContent(raw, "Recent requests")
	if len(c) != 11 {
		t.Fatalf("Recent requests: %d content lines, want header + 10:\n%s", len(c), strings.Join(plain, "\n"))
	}
	head := stripANSI(c[0])
	for _, col := range []string{"time", "slot", "prompt", "cached", "generated", "duration", "tok/s", "ttft"} {
		if !strings.Contains(head, col) {
			t.Errorf("header lacks %q: %q", col, head)
		}
	}
	// newest first: the first row is Recent[0], the tenth Recent[9]; times are local
	for i, want := range []int{0, 9} {
		row := stripANSI(c[1+want])
		if !strings.Contains(row, s.Recent[want].At.Local().Format("15:04:05")) {
			t.Errorf("row %d %q lacks the time of Recent[%d]", i, row, want)
		}
	}
	if row := stripANSI(c[2]); strings.Count(row, "~") != 2 { // duration and ttft of the Approx row
		t.Errorf("approx row: %q", row)
	}
	if row := stripANSI(c[1]); strings.Contains(row, "~") || !strings.Contains(row, "0.8s") || !strings.Contains(row, "83.4") {
		t.Errorf("exact row: %q", row)
	}
	if row := stripANSI(c[3]); strings.Count(row, " -") != 2 { // zero GenRate and TTFT
		t.Errorf("zero rate/ttft row: %q", row)
	}
	// Recent sits between Requests and Throughput
	if !(titleLine(plain, "┌ Requests") < titleLine(plain, "┌ Recent requests") &&
		titleLine(plain, "┌ Recent requests") < titleLine(plain, "┌ Throughput")) {
		t.Errorf("Recent requests out of order:\n%s", strings.Join(plain, "\n"))
	}
	// two requests: both shown (fewer than 10)
	s.Recent = s.Recent[:2]
	if c := panelContent(lines(Render(s, o)), "Recent requests"); len(c) != 3 {
		t.Errorf("2 recent: %d lines", len(c))
	}
	s.Recent = nil
	if out := Render(s, o); strings.Contains(out, "Recent requests") {
		t.Errorf("Recent panel with no requests:\n%s", stripANSI(out))
	}
}

func TestRenderHost(t *testing.T) {
	s := fullSnapshot(t)
	out := stripANSI(Render(s, opts(135, 120, s)))
	for _, want := range []string{"┌ Host", "64 cores", "load 1.14 1.15 1.10", "18.6/251.5 GB", "swap 0.0/1.0 GB",
		"1.5T free of 1.8T"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	ls := lines(out)
	cpu, ram := ls[titleLine(ls, "64 cores")], ls[titleLine(ls, "18.6/251.5 GB")]
	if a, b := strings.Index(cpu, "█"), strings.Index(ram, "█"); a < 0 || a != b {
		t.Errorf("host bars not aligned:\n%s\n%s", cpu, ram)
	}
	s.Host = nil
	if out := Render(s, opts(135, 120, s)); strings.Contains(out, "Host") {
		t.Errorf("Host panel without Host:\n%s", stripANSI(out))
	}
	// RAM over 90%: amber bar
	s.Host = hostFixture()
	s.Host.MemAvailKB = s.Host.MemTotalKB / 20
	raw := lines(Render(s, opts(135, 120, s)))
	c := panelContent(raw, "Host")
	if len(c) < 2 || !strings.Contains(c[1], fgSGR(221)+"█") {
		t.Errorf("RAM bar not amber at 95%%: %q", c)
	}
	// without colour the warning is the "!" the GPU rows use
	ramRow := func(o Options) string { return panelContent(lines(Render(s, o)), "Host")[1] }
	nc := opts(135, 120, s)
	nc.NoColor = true
	if row := ramRow(nc); !strings.Contains(row, "239.0/251.5 GB!") {
		t.Errorf("RAM 95%% NoColor: %q", row)
	}
	as := opts(135, 120, s)
	as.ASCII = true
	if out := Render(s, as); !strings.Contains(out, "239.0/251.5 GB!") { // ASCII boxes start with "+", not "┌"
		t.Errorf("RAM 95%% ASCII:\n%s", out)
	}
	if row := ramRow(opts(135, 120, s)); strings.Contains(row, "!") {
		t.Errorf("RAM 95%% in colour has a marker: %q", row)
	}
	s.Host = hostFixture()
	o := opts(135, 120, s)
	o.NoColor = true
	if row := ramRow(o); strings.Contains(row, "!") {
		t.Errorf("RAM 7%% NoColor: %q", row)
	}
}

// fgSGR is the start of a palette256 foreground sequence for colour n.
func fgSGR(n int) string { return fmt.Sprintf("38;5;%dm", n) }

func TestRenderProcAndContainer(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	raw := Render(s, o)
	out := stripANSI(raw)
	for _, want := range []string{"┌ llama-server", "pid 115429", "cpu 105% (of one core)", "mem 11.7 GB", "threads 103",
		"container llama-server-main", "running", "unhealthy", "up 3h12m", "restarts 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(raw, fgSGR(203)+"unhealthy") {
		t.Errorf("unhealthy not red")
	}
	s.Container = nil
	out = stripANSI(Render(s, o))
	if !strings.Contains(out, "pid 115429") || strings.Contains(out, "container") {
		t.Errorf("proc only:\n%s", out)
	}
	s.Container = containerFixture()
	s.Container.Health, s.Container.State = "", "exited"
	raw = Render(s, o)
	if out := stripANSI(raw); !strings.Contains(out, "no healthcheck") || strings.Contains(out, "up ") {
		t.Errorf("no healthcheck / exited:\n%s", out)
	}
	if !strings.Contains(raw, fgSGR(203)+"exited") {
		t.Errorf("exited not red")
	}
	s.Container.Health, s.Container.State = "starting", "running"
	if raw := Render(s, o); !strings.Contains(raw, fgSGR(221)+"starting") {
		t.Errorf("starting not amber")
	}
	// started in the future (clock skew): never a negative uptime
	s.Container.StartedAt = s.At.Add(time.Hour)
	if out := stripANSI(Render(s, o)); !strings.Contains(out, "up 0s") {
		t.Errorf("future start:\n%s", out)
	}
	s.Proc, s.Container = nil, nil
	if out := Render(s, o); strings.Contains(out, "llama-server") {
		t.Errorf("llama-server panel with nothing to show:\n%s", stripANSI(out))
	}
}

func TestFmtUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Hour: "0s", 42 * time.Second: "42s", 44 * time.Minute: "44m",
		3*time.Hour + 12*time.Minute + 59*time.Second: "3h12m", 52 * time.Hour: "2d4h"} {
		if got := fmtUptime(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

func TestRenderLocalStale(t *testing.T) {
	s := fullSnapshot(t)
	s.HostAt, s.ProcAt = s.At.Add(-5*time.Second), s.At.Add(-5*time.Second)
	s.ContainerAt = s.At.Add(-25 * time.Second) // container threshold is 3 x 10s: fresh
	raw := lines(Render(s, opts(135, 120, s)))
	plain := lines(stripANSI(strings.Join(raw, "\n")))
	if title := plain[titleLine(plain, "┌ Host")]; !strings.Contains(title, "stale, 5s ago") || !dimmed(panelContent(raw, "Host")) {
		t.Errorf("host not stale: %q", title)
	}
	c := panelContent(raw, "llama-server")
	title := plain[titleLine(plain, "┌ llama-server")]
	if !strings.Contains(title, "process stale, 5s ago") || !dimmed(c[:1]) || dimmed(c[1:]) {
		t.Errorf("process stale: %q", title)
	}
	s.HostAt, s.ProcAt, s.ContainerAt = s.At, s.At, s.At.Add(-45*time.Second)
	raw = lines(Render(s, opts(135, 120, s)))
	plain = lines(stripANSI(strings.Join(raw, "\n")))
	c = panelContent(raw, "llama-server")
	if title := plain[titleLine(plain, "┌ llama-server")]; !strings.Contains(title, "container stale, 45s ago") || dimmed(c[:1]) || !dimmed(c[1:]) {
		t.Errorf("container stale: %q", title)
	}
	if title := plain[titleLine(plain, "┌ Host")]; strings.Contains(title, "stale") {
		t.Errorf("fresh host tagged: %q", title)
	}
	s.ProcAt = s.At.Add(-5 * time.Second)
	plain = lines(stripANSI(Render(s, opts(135, 120, s))))
	if title := plain[titleLine(plain, "┌ llama-server")]; !strings.Contains(title, "stale, 45s ago") || strings.Contains(title, "process") {
		t.Errorf("both stale: %q", title)
	}
}

func TestRenderGPUWarnings(t *testing.T) {
	s := fullSnapshot(t)
	tempRow := func(o Options) string {
		ls := lines(Render(s, o))
		c := panelContent(ls, "GPU 0")
		if len(c) < 3 {
			t.Fatalf("GPU panel: %q", c)
		}
		return c[2]
	}
	o := opts(135, 120, s)
	nc := o
	nc.NoColor = true
	as := o
	as.ASCII = true
	if row := tempRow(o); !strings.Contains(row, fgSGR(231)+"64°C") || strings.Contains(row, "!") {
		t.Errorf("64C: %q", row)
	}
	if row := stripANSI(tempRow(o)); !strings.Contains(row, "fan 33%") {
		t.Errorf("fan: %q", row)
	}
	s.GPUs[0].TempC = 85
	if row := tempRow(o); !strings.Contains(row, fgSGR(221)+"85°C") || strings.Contains(row, "!") {
		t.Errorf("85C colour: %q", row)
	}
	if row := tempRow(nc); !strings.Contains(row, "85°C!") || strings.Contains(row, "!!") {
		t.Errorf("85C NoColor: %q", row)
	}
	s.GPUs[0].TempC = 92
	if row := tempRow(o); !strings.Contains(row, fgSGR(203)+"92°C") {
		t.Errorf("92C colour: %q", row)
	}
	if out := Render(s, as); !strings.Contains(out, "92C!!") { // ASCII boxes start with "+", not "┌"
		t.Errorf("92C ASCII: %q", out)
	}
	s.GPUs[0].TempC = 64.37
	s.GPUs[0].MemUsedMiB = 0.96 * s.GPUs[0].MemTotalMiB
	vram := func(o Options) string { return panelContent(lines(Render(s, o)), "GPU 0")[1] }
	if row := vram(o); !strings.Contains(row, fgSGR(221)+"30.6/31.8 GB") || !strings.Contains(row, fgSGR(221)+"█") {
		t.Errorf("VRAM 96%%: %q", row)
	}
	if row := vram(nc); !strings.Contains(row, "30.6/31.8 GB!") {
		t.Errorf("VRAM 96%% NoColor: %q", row)
	}
	s.GPUs[0].MemUsedMiB = 29965
	if row := vram(nc); strings.Contains(row, "!") {
		t.Errorf("VRAM 92%% NoColor: %q", row)
	}
	s.GPUs[0].HasFan = false
	if out := stripANSI(strings.Join(panelContent(lines(Render(s, o)), "GPU 0"), "\n")); strings.Contains(out, "fan ") {
		t.Errorf("fan without HasFan")
	}
}

// gpuHists returns 60 GPU samples (one per 2s GPU query over the realistic
// snapshot's two minutes): utilisation, VRAM % and temperature following the load.
func gpuHists() (util, mem, temp []float64) {
	gen, prompt, _ := realistic()
	for i := 0; i < 120; i += 2 {
		u, tc := 2.0, 41.0
		if gen[i] > 0 || prompt[i] > 0 {
			u, tc = 93-float64(i%5), 62+float64(i%7)*0.4
		}
		util, mem, temp = append(util, u), append(mem, 91.9), append(temp, tc)
	}
	return util, mem, temp
}

// The warning colour and the !/!! markers follow the rounded value on screen.
func TestGPUTempWarnRounded(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	nc := o
	nc.NoColor = true
	for _, c := range []struct {
		temp       float64
		shown, sgr string
		mark       string
	}{
		{79.4, "79°C", fgSGR(231), ""}, {79.5, "80°C", fgSGR(221), "!"},
		{89.4, "89°C", fgSGR(221), "!"}, {89.5, "90°C", fgSGR(203), "!!"},
	} {
		s.GPUs[0].TempC = c.temp
		row := panelContent(lines(Render(s, o)), "GPU 0")[2]
		if !strings.Contains(row, c.sgr+c.shown) {
			t.Errorf("%v: %q, want %s in colour %s", c.temp, stripANSI(row), c.shown, c.sgr)
		}
		row = panelContent(lines(Render(s, nc)), "GPU 0")[2]
		if !strings.Contains(row, c.shown+c.mark+" ") {
			t.Errorf("%v NoColor: %q, want %q", c.temp, row, c.shown+c.mark)
		}
	}
}

func TestRecentTitleExplainsMarkers(t *testing.T) {
	s := fullSnapshot(t)
	long := "newest first, ~ approximate, - not measured"
	if title := panelTitle(stripANSI(Render(s, opts(135, 120, s))), "Recent requests"); !strings.Contains(title, long) {
		t.Errorf("135: %q", title)
	}
	title := panelTitle(stripANSI(Render(s, opts(60, 120, s))), "Recent requests")
	if strings.Contains(title, "approximate") || !strings.Contains(title, "newest first") {
		t.Errorf("60: %q", title)
	}
}

func TestRenderCostLine(t *testing.T) {
	s := fullSnapshot(t)
	s.Cost = collect.Cost{Enabled: true, Currency: "GBP", Price: 0.3031, SinceStart: 0.0042, PerDay: 1.76, PerMTok: 0.021}
	out := stripANSI(Render(s, opts(135, 120, s)))
	for _, want := range []string{"cost", "0.4p since start", "£1.76/day at average draw", "2.1p per 1M tokens avg",
		"(gpu+cpu, 30.31p/kWh)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// the other symbol currencies: the minor unit with two decimals; ASCII mode
	// spells the symbol
	s.Cost.Currency = "EUR"
	o := opts(135, 120, s)
	o.ASCII = true
	if out := Render(s, o); !strings.Contains(out, "(gpu+cpu, 30.31c/kWh)") || !strings.Contains(out, "EUR 1.76/day") {
		t.Errorf("EUR ASCII:\n%s", out)
	}
	s.Cost.Currency = "USD"
	if out := stripANSI(Render(s, opts(135, 120, s))); !strings.Contains(out, "(gpu+cpu, 30.31c/kWh)") {
		t.Errorf("USD:\n%s", out)
	}
	// other currencies: Format(price)/kWh
	s.Cost.Currency, s.HasCPUPower = "CHF", false
	if out := stripANSI(Render(s, opts(135, 120, s))); !strings.Contains(out, "(gpu, 0.30 CHF/kWh)") {
		t.Errorf("CHF:\n%s", out)
	}
	s.Cost = collect.Cost{}
	s.Today, s.Month, s.AllTime = store.Totals{}, store.Totals{}, store.Totals{}
	if out := Render(s, opts(135, 120, s)); strings.Contains(out, "cost") {
		t.Errorf("cost line when cost is off:\n%s", stripANSI(out))
	}
}

// --frame covers about a second: the Energy panel has no "since start" figures and
// prices the day at the current draw.
func TestRenderEnergyOneShot(t *testing.T) {
	s := fullSnapshot(t)
	s.OneShot = true
	for _, w := range []int{135, 80} {
		out := stripANSI(Render(s, opts(w, 120, s)))
		c := strings.Join(panelContent(lines(out), "Energy"), "\n")
		if !strings.Contains(c, "£3.48/day at current draw") || !strings.Contains(c, "draw") {
			t.Errorf("%d: Energy panel:\n%s", w, c)
		}
		for _, gone := range []string{"since start", "average draw", "per 1M"} {
			if strings.Contains(c, gone) {
				t.Errorf("%d: one-shot Energy panel has %q:\n%s", w, gone, c)
			}
		}
	}
}

func TestRenderTotals(t *testing.T) {
	s := fullSnapshot(t)
	out := stripANSI(Render(s, opts(135, 120, s)))
	ls := lines(out)
	today, month, all := titleLine(ls, "│ today"), titleLine(ls, "│ month"), titleLine(ls, "│ all time")
	if today < 0 || month != today+1 || all != month+1 {
		t.Fatalf("totals block %d %d %d:\n%s", today, month, all, out)
	}
	for _, want := range []string{"11.5 kWh", "£3.49", "1.2M gen tokens", "42 requests"} {
		if !strings.Contains(ls[today], want) {
			t.Errorf("today lacks %q: %q", want, ls[today])
		}
	}
	if !strings.Contains(ls[all], "871 kWh") || !strings.Contains(ls[all], "3120 requests") {
		t.Errorf("all time: %q", ls[all])
	}
	// columns line up
	if a, b := strings.Index(ls[today], "requests"), strings.Index(ls[all], "requests"); a != b {
		t.Errorf("columns not aligned:\n%s\n%s", ls[today], ls[all])
	}
	s.Cost = collect.Cost{}
	ls = lines(stripANSI(Render(s, opts(135, 120, s))))
	if l := ls[titleLine(ls, "│ today")]; strings.Contains(l, "£") || !strings.Contains(l, "42 requests") {
		t.Errorf("cost off: %q", l)
	}
	s.Today, s.Month, s.AllTime = store.Totals{}, store.Totals{}, store.Totals{}
	if out := stripANSI(Render(s, opts(135, 120, s))); strings.Contains(out, "today") || strings.Contains(out, "all time") {
		t.Errorf("totals without data:\n%s", out)
	}
}

func TestRenderDBStatus(t *testing.T) {
	s := fullSnapshot(t)
	s.DBStatus = "off (locked)"
	raw := lines(Render(s, opts(135, 120, s)))
	if h := stripANSI(raw[0]); !strings.Contains(h, "db: off (locked)") {
		t.Errorf("header: %q", h)
	}
	if !strings.Contains(raw[0], fgSGR(221)+"db: off (locked)") {
		t.Errorf("db status not in the warning colour: %q", raw[0])
	}
	s.DBStatus = ""
	if h := Render(s, opts(135, 120, s)); strings.Contains(lines(h)[0], "db:") {
		t.Errorf("db status when healthy")
	}
}

// A second dashboard on a database another one records to says so in the header,
// in the normal colour, shortened when the line is full.
func TestRenderDBReadOnly(t *testing.T) {
	s := fullSnapshot(t)
	s.DBReadOnly = true
	long := "db: read-only (another llamadash is recording)"
	raw := lines(Render(s, opts(135, 120, s)))
	if h := stripANSI(raw[0]); !strings.Contains(h, long) || !strings.Contains(h, "refresh 1s") {
		t.Errorf("135 header: %q", h)
	}
	if strings.Contains(raw[0], fgSGR(221)+"db:") || !strings.Contains(raw[0], fgSGR(250)+long) {
		t.Errorf("read-only note not in the normal colour: %q", raw[0])
	}
	h := stripANSI(lines(Render(s, opts(80, 24, s)))[0])
	if !strings.Contains(h, "db: read-only") || strings.Contains(h, "another") || len([]rune(h)) != 80 ||
		!strings.Contains(h, "llamadash │ example-model-27b") {
		t.Errorf("80 header: %q", h)
	}
	s.DBStatus = "off (locked)" // a database error is the one shown
	if h := stripANSI(lines(Render(s, opts(135, 120, s)))[0]); !strings.Contains(h, "db: off (locked)") || strings.Contains(h, "read-only") {
		t.Errorf("error and read-only: %q", h)
	}
}

// Paused at 80 columns nothing is cut: the refresh rate goes first, then the long
// paused form, then the long read-only note; health is never cut or dropped.
func TestHeaderPausedNarrow(t *testing.T) {
	s := fullSnapshot(t)
	header := func(s collect.Snapshot, w int) string {
		o := opts(w, 24, s)
		o.Paused, o.History = true, true
		h := stripANSI(lines(Render(s, o))[0])
		if n := len([]rune(h)); n != w {
			t.Errorf("header %d wide: %q", n, h)
		}
		return h
	}
	h := header(s, 80)
	if !strings.HasPrefix(h, "llamadash │ example-model-27b │ ctx 262.1k × 1 slot │ ok ") ||
		!strings.Contains(h, " paused ") || strings.Contains(h, "(still recording)") {
		t.Errorf("paused, recording, 80: %q", h)
	}
	if h := header(s, 135); !strings.Contains(h, "paused (still recording)") || !strings.Contains(h, "refresh 1s") {
		t.Errorf("paused, recording, 135: %q", h)
	}
	s.DBReadOnly = true
	if h := header(s, 80); !strings.Contains(h, "│ ok │ db: read-only ") || !strings.HasSuffix(h, " paused ") {
		t.Errorf("paused, read-only, 80: %q", h)
	}
	s.Health.Status = "no slot available"
	if h := header(s, 80); !strings.Contains(h, "│ no slot available │ db: read-only ") || !strings.HasSuffix(h, " paused ") ||
		!strings.HasPrefix(h, "llamadash │ ") {
		t.Errorf("paused, read-only, health not ok, 80: %q", h)
	}
}

// everything is fullSnapshot with 8 slots, 2 GPUs, the containers and a database warning.
func everything(t *testing.T) collect.Snapshot {
	s := withContainers(fullSnapshot(t))
	base := s.Slots[0]
	s.Slots = nil
	for i := 0; i < 8; i++ {
		v := base
		v.ID = i
		if i%3 == 1 {
			v.State = stats.Idle
		}
		s.Slots = append(s.Slots, v)
	}
	g1 := rtx5090()
	g1.Index, g1.HasFan = 1, false
	s.GPUs = append(s.GPUs, g1)
	s.DBStatus = "off (locked)"
	return s
}

func TestRenderFitsWidthAllPanels(t *testing.T) {
	s := everything(t)
	for _, w := range []int{0, 1, 20, 40, 79, 80, 120, 135, 300} {
		for _, h := range []int{0, 1, 8, 24, 60, 120} {
			for _, mode := range []Options{{}, {Color16: true}, {ASCII: true}, {NoColor: true}, {Paused: true}} {
				o := opts(w, h, s)
				o.Color16, o.ASCII, o.NoColor, o.Paused = mode.Color16, mode.ASCII, mode.NoColor, mode.Paused
				out := Render(s, o)
				checkFits(t, "everything", out, o)
				ls := lines(stripANSI(out))
				if w == 0 || h == 0 {
					if out != "" {
						t.Errorf("%dx%d: output", w, h)
					}
					continue
				}
				if len(ls) != h {
					t.Errorf("%dx%d: %d lines", w, h, len(ls))
				}
				for i, l := range ls {
					if lw := len([]rune(l)); lw != w {
						t.Errorf("%dx%d line %d is %d wide", w, h, i, lw)
					}
				}
				if h > 1 && !strings.HasPrefix(ls[h-1], "q quit"[:min(w, 6)]) {
					t.Errorf("%dx%d: footer not last: %q", w, h, ls[h-1])
				}
			}
		}
	}
}

// present reports which sections the frame shows.
func present(out string) map[string]bool {
	m := map[string]bool{}
	for _, p := range []string{"Requests", "Recent requests", "Throughput", "GPU 0", "Host", "llama-server", "Containers",
		"Energy", "generation tok/s", "power", "prompt tok/s"} {
		m[p] = strings.Contains(out, "┌ "+p+" ")
	}
	return m
}

func TestRenderDropOrder(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	if p := present(stripANSI(Render(s, opts(135, 40, s)))); !p["Requests"] || !p["Throughput"] || !p["GPU 0"] || !p["Energy"] {
		t.Fatalf("135x40: %v", p)
	}
	// each section, once gone as the height shrinks, stays gone; and the order in
	// which they go is the chart grid (power is in its last row, the filler chart
	// that may use rows left over later is generation only), Recent requests,
	// Containers, llama-server, Host
	order := []string{"power", "Recent requests", "Containers", "llama-server", "Host"}
	gone := map[string]int{}
	for h := 60; h >= 24; h-- {
		p := present(stripANSI(Render(s, opts(135, h, s))))
		for _, name := range order {
			if _, ok := gone[name]; ok && p[name] {
				t.Errorf("%s back at height %d", name, h)
			}
			if _, ok := gone[name]; !ok && !p[name] {
				gone[name] = h
			}
		}
		if !p["Requests"] || !p["Throughput"] || !p["GPU 0"] || !p["Energy"] {
			t.Errorf("135x%d: version 1 panel missing: %v", h, p)
		}
	}
	for i, name := range order {
		if _, ok := gone[name]; !ok {
			t.Fatalf("%s never dropped down to height 24 (%v)", name, gone)
		}
		if i > 0 && gone[name] >= gone[order[i-1]] {
			t.Errorf("%s dropped at %d, before %s (%d)", name, gone[name], order[i-1], gone[order[i-1]])
		}
	}
	// Recent shrinks to 3 rows before it goes
	minRows := 99
	for h := 60; h >= 24; h-- {
		if c := panelContent(lines(Render(s, opts(135, h, s))), "Recent requests"); len(c) > 0 {
			minRows = min(minRows, len(c)-1)
		}
	}
	if minRows != 3 {
		t.Errorf("Recent requests shrank to %d rows, want 3", minRows)
	}
}

func TestRenderPortraitFull(t *testing.T) {
	s := withContainers(fullSnapshot(t))
	for _, color16 := range []bool{false, true} {
		o := opts(135, 120, s)
		o.Color16 = color16
		out := stripANSI(Render(s, o))
		checkFits(t, "portrait full", out, o)
		ls := lines(out)
		if len(ls) != 120 || !strings.HasPrefix(ls[119], "q quit") {
			t.Fatalf("%d lines, last %q", len(ls), ls[len(ls)-1])
		}
		for i := 1; i < len(ls); i++ {
			if strings.TrimSpace(ls[i]) == "" && strings.TrimSpace(ls[i-1]) == "" {
				t.Fatalf("blank band at line %d:\n%s", i, out)
			}
		}
		for name, ok := range present(out) {
			if !ok {
				t.Errorf("missing section %q:\n%s", name, out)
			}
		}
		prev := -1
		for _, name := range []string{"Requests", "Recent requests", "Throughput", "GPU 0", "Host", "llama-server", "Containers", "Energy",
			"generation tok/s"} {
			i := titleLine(ls, "┌ "+name+" ")
			if i <= prev {
				t.Errorf("%s at line %d, not after line %d", name, i, prev)
			}
			prev = i
		}
	}
}

func TestRenderNoSecretsNew(t *testing.T) {
	s := fullSnapshot(t)
	s.Container.Name = "evil\x1b[31mname\x1b]0;title\x07"
	s.Container.Health = "\x1b[2Jbad"
	s.Container.State = "run\x1bning"
	s.Recent[0].SlotID = -1
	for _, mode := range []Options{{}, {Color16: true}, {NoColor: true}, {ASCII: true}} {
		o := opts(135, 120, s)
		o.Color16, o.NoColor, o.ASCII = mode.Color16, mode.NoColor, mode.ASCII
		out := Render(s, o)
		if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x07") || strings.Contains(out, "\x1b[2J") ||
			strings.Contains(out, "\x1b[31mname") {
			t.Fatalf("raw escape from data in the output")
		}
		if !strings.Contains(stripANSI(out), "evil?[31mname?]0;title?") {
			t.Fatalf("name not neutralised:\n%s", stripANSI(out))
		}
	}
	plain := RenderPlain(s)
	assertPlain(t, plain)
}

// TestSyntheticFrames writes the console-mode frames of the realistic snapshot
// to $LLAMADASH_FRAME_DIR when set (used to review the layout): live at three
// sizes, and the 24h range from a store holding a synthetic day.
func TestSyntheticFrames(t *testing.T) {
	s := fullSnapshot(t)
	write := func(name, out string) {
		if d := os.Getenv("LLAMADASH_FRAME_DIR"); d != "" {
			if err := os.WriteFile(filepath.Join(d, name), []byte(stripANSI(out)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, sz := range [][2]int{{135, 120}, {100, 40}, {80, 24}, {135, 48}, {80, 36}} {
		o := opts(sz[0], sz[1], s)
		o.Color16, o.History = true, true
		out := Render(s, o)
		checkFits(t, "synthetic", out, o)
		write(fmt.Sprintf("frame3-%dx%d-live.txt", sz[0], sz[1]), out)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "day.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, x := range syntheticDay(s.At) {
		st.AddSample(x)
		if i%1000 == 999 { // the store queues 1024 samples at most
			if err := st.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.Flush(); err != nil {
		t.Fatal(err)
	}
	o := opts(135, 120, s)
	o.Color16, o.History, o.Range = true, true, Range24h
	o = LoadHistory(st, s, o, 10*time.Second)
	out := Render(s, o)
	checkFits(t, "synthetic 24h", out, o)
	if o.LongErr || !strings.Contains(stripANSI(out), "24h, avg per column") {
		t.Fatalf("24h frame:\n%s", stripANSI(out))
	}
	write("frame3-135x120-24h.txt", out)
}

// syntheticDay is a day of 10 s samples ending at end: requests in bursts, busier
// in the working day, and the dashboard stopped from 02:10 to 05:40.
func syntheticDay(end time.Time) []store.Sample {
	var out []store.Sample
	busy, left := false, 0
	seed := uint32(7)
	rnd := func() float64 { // xorshift: the same day every run
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		return float64(seed%10000) / 10000
	}
	for at := end.Add(-24 * time.Hour); !at.After(end); at = at.Add(10 * time.Second) {
		hour := at.Hour()
		if mins := hour*60 + at.Minute(); mins >= 2*60+10 && mins < 5*60+40 {
			continue
		}
		if left--; left <= 0 {
			pBusy := 0.25
			if hour >= 9 && hour < 19 {
				pBusy = 0.6
			}
			busy = rnd() < pBusy
			left = 6 + int(rnd()*60) // one to eleven minutes
		}
		x := store.Sample{At: at, GPUPowerW: 62, CPUPowerW: 34, GPUUtil: 1, GPUMemPct: 91.9, GPUTempC: 41,
			HostCPUPct: 2.5 + rnd()}
		if busy {
			x.GenRate = 70 + rnd()*16
			x.PromptRate = 300 * rnd()
			x.GPUPowerW, x.CPUPowerW = 420+rnd()*40, 110+rnd()*15
			x.GPUUtil, x.GPUTempC, x.HostCPUPct = 88+rnd()*8, 60+rnd()*6, 4+rnd()*2
		}
		out = append(out, x)
	}
	return out
}

// Review-frame findings: the Energy panel sheds its totals, then its cost line,
// before it is dropped; the cost line sheds words rather than being cut off.
func TestRenderEnergyCompacts(t *testing.T) {
	s := fullSnapshot(t)
	out := stripANSI(Render(s, opts(80, 24, s)))
	if !strings.Contains(out, "┌ Energy") || !strings.Contains(out, "cost ") || strings.Contains(out, "today") {
		t.Errorf("80x24: Energy without totals expected:\n%s", out)
	}
	out = stripANSI(Render(s, opts(80, 23, s)))
	if !strings.Contains(out, "┌ Energy") || strings.Contains(out, "cost ") {
		t.Errorf("80x23: Energy without cost expected:\n%s", out)
	}
	out = stripANSI(Render(s, opts(100, 40, s)))
	if !strings.Contains(out, "(gpu+cpu, 30.31p/kWh)") || !strings.Contains(out, "£3.48/day") || strings.Contains(out, "average draw") {
		t.Errorf("100x40 cost line:\n%s", out)
	}
	out = stripANSI(Render(s, opts(60, 40, s)))
	if !strings.Contains(out, "46p since start   £3.48/day ") || strings.Contains(out, "per 1M") || strings.Contains(out, "kWh)") {
		t.Errorf("60x40 cost line:\n%s", out)
	}
}

// Rows from before the snapshot's local midnight carry their date.
func TestRecentEarlierDays(t *testing.T) {
	defer func(l *time.Location) { time.Local = l }(time.Local)
	time.Local = time.FixedZone("UTC+2", 2*3600)
	s := fullSnapshot(t)
	s.At = time.Date(2026, 11, 25, 1, 0, 0, 0, time.Local)
	s.Recent = recentFixture(3, s.At)
	s.Recent[1].At = time.Date(2026, 11, 25, 0, 0, 0, 0, time.Local)          // today, at midnight
	s.Recent[2].At = time.Date(2026, 11, 24, 23, 59, 59, 0, time.Local).UTC() // yesterday, in another zone
	for _, w := range []int{135, 80} {
		c := panelContent(lines(stripANSI(Render(s, opts(w, 120, s)))), "Recent requests")
		row := func(i int) string { return strings.TrimPrefix(c[i], "│ ") }
		if len(c) != 4 || !strings.HasPrefix(row(1), "01:00:00 ") || !strings.HasPrefix(row(2), "00:00:00 ") ||
			!strings.HasPrefix(row(3), "11-24 23:59 ") {
			t.Errorf("%d: rows\n%s", w, strings.Join(c, "\n"))
		}
		if w == 80 && !strings.Contains(c[3], "generated") && !strings.Contains(c[0], "generated") {
			t.Errorf("80: the generated column was dropped:\n%s", strings.Join(c, "\n"))
		}
	}
	s.Recent = s.Recent[2:]
	if out := RenderPlain(s); !strings.Contains(out, "latest 11-24 23:59 slot 0") {
		t.Errorf("plain:\n%s", out)
	}
}

func TestRecentLayoutGap(t *testing.T) {
	if cols, gap := recentLayout(131); len(cols) != nRecentCols || gap != 4 {
		t.Errorf("131: %d cols gap %d", len(cols), gap)
	}
	if cols, _ := recentLayout(40); len(cols) >= nRecentCols || cols[0] != rTime {
		t.Errorf("40: %v", cols)
	}
}

func TestRenderTotalsNarrow(t *testing.T) {
	s := fullSnapshot(t)
	ls := lines(stripANSI(Render(s, opts(60, 60, s))))
	l := ls[titleLine(ls, "│ all time")]
	if !strings.Contains(l, "88.7M gen") || !strings.Contains(l, "3120 req ") {
		t.Errorf("narrow totals: %q", l)
	}
}

// The container's health word is coloured by its meaning; without colour starting
// and unhealthy carry the ! / !! markers. Other values keep the value colour.
func TestContainerHealthColour(t *testing.T) {
	for _, c := range []struct {
		health, sgr, plain string
	}{
		{"healthy", fgSGR(114), "running   healthy   up"},
		{"starting", fgSGR(221), "running   starting!   up"},
		{"unhealthy", fgSGR(203), "running   unhealthy!!   up"},
		{"weird", fgSGR(231), "running   weird   up"},
	} {
		s := fullSnapshot(t)
		s.Container.Health = c.health
		row := func(o Options) string {
			for _, l := range lines(Render(s, o)) {
				if strings.Contains(stripANSI(l), "container llama") {
					return l
				}
			}
			return ""
		}
		o := opts(135, 120, s)
		if got := row(o); !strings.Contains(got, c.sgr+c.health) || strings.Contains(stripANSI(got), c.health+"!") {
			t.Errorf("%s: %q, want colour %s and no marker", c.health, got, c.sgr)
		}
		for _, mode := range []Options{{NoColor: true}, {ASCII: true}} {
			m := o
			m.NoColor, m.ASCII = mode.NoColor, mode.ASCII
			if got := stripANSI(row(m)); !strings.Contains(got, c.plain) {
				t.Errorf("%s NoColor=%v ASCII=%v: %q, want %q", c.health, m.NoColor, m.ASCII, got, c.plain)
			}
			checkFits(t, c.health, Render(s, m), m)
		}
	}
}
