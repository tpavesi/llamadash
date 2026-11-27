package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/fakeserver"
	"llamadash/internal/gpu"
	"llamadash/internal/llama"
	"llamadash/internal/stats"
	"llamadash/internal/store"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixtureSnapshot(t *testing.T) collect.Snapshot {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	c := collect.New(collect.Config{URL: srv.URL, Interval: time.Second, GPU: collect.GPUOff})
	var s collect.Snapshot
	for i := 0; i < 3; i++ {
		s = c.Poll(context.Background(), t0.Add(time.Duration(i)*time.Second))
	}
	return s
}

func assertPlain(t *testing.T, out string) {
	t.Helper()
	for _, r := range out {
		if r > 127 || r == 0x1b {
			t.Fatalf("non-plain rune %q in output:\n%s", r, out)
		}
	}
}

func TestRenderPlainBusy(t *testing.T) {
	out := RenderPlain(fixtureSnapshot(t))
	for _, want := range []string{"example-model-27b", "262.1k", "generating", "tok/s"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	assertPlain(t, out)
}

func TestRenderPlainNoSecrets(t *testing.T) {
	props := `{"total_slots":1,"model_alias":"m","build_info":"b1","endpoint_slots":true,"endpoint_metrics":false,
	 "chat_template":"TEMPLATE_SENTINEL","default_generation_settings":{"n_ctx":4096,"params":{"generation_prompt":"PROMPT_SENTINEL"}},
	 "media_marker":"TEMPLATE_SENTINEL"}`
	slots := `[{"id":0,"n_ctx":4096,"is_processing":true,"id_task":1,"n_prompt_tokens":100,"n_prompt_tokens_processed":10,"n_prompt_tokens_cache":90,
	 "params":{"generation_prompt":"PROMPT_SENTINEL"},"next_token":[{"n_decoded":5,"n_remain":-1}]}]`
	srv := fakeserver.New([]fakeserver.Step{{
		Health:  fakeserver.Response{Status: 200, Body: []byte(`{"status":"ok"}`)},
		Props:   fakeserver.Response{Status: 200, Body: []byte(props)},
		Slots:   fakeserver.Response{Status: 200, Body: []byte(slots)},
		Metrics: fakeserver.Response{Status: 501},
	}})
	defer srv.Close()
	c := collect.New(collect.Config{URL: srv.URL, Interval: time.Second, GPU: collect.GPUOff})
	s := c.Poll(context.Background(), t0)
	out := RenderPlain(s)
	if !strings.Contains(out, "generating") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if strings.Contains(out, "SENTINEL") {
		t.Fatalf("leaked secret:\n%s", out)
	}
}

func TestRenderPlainHints(t *testing.T) {
	s := collect.Snapshot{Reachable: true, SlotsHint: "slots endpoint is disabled: start llama-server with --slots",
		MetricsHint: "start llama-server with --metrics for queue depth and totals"}
	out := RenderPlain(s)
	if !strings.Contains(out, s.SlotsHint) || !strings.Contains(out, s.MetricsHint) {
		t.Fatalf("hints missing:\n%s", out)
	}
	down := RenderPlain(collect.Snapshot{URL: "http://x", UnreachableSince: t0})
	if !strings.Contains(down, "unreachable since") {
		t.Fatalf("no unreachable text:\n%s", down)
	}
}

func TestRenderPlainPowerAndGPU(t *testing.T) {
	s := collect.Snapshot{Reachable: true, HasPower: true, PowerW: 285, EnergyWh: 420, JoulesPerToken: 7.5,
		GPUs: []gpu.GPU{{Index: 0, Name: "RTX", UtilPct: 90, HasUtil: true, MemUsedMiB: 1000, MemTotalMiB: 24000, TempC: 61, HasTemp: true}}}
	out := RenderPlain(s)
	for _, want := range []string{"power 285 W   energy 420 Wh since start   7.5 J/token", "GPU 0 (this host)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	s.EnergyWh, s.JoulesPerToken = 1234, 0
	out = RenderPlain(s)
	if !strings.Contains(out, "energy 1.23 kWh since start") || strings.Contains(out, "J/token") {
		t.Errorf("kWh/omit rule:\n%s", out)
	}
	s.HasPower = false
	if strings.Contains(RenderPlain(s), "power ") {
		t.Error("power line without HasPower")
	}
}

// --once covers about a second: no "since start" figures, and the per-day cost is
// at the current draw.
func TestRenderPlainOneShot(t *testing.T) {
	s := fullSnapshot(t)
	s.OneShot = true
	out := RenderPlain(s)
	for _, want := range []string{"power 578 W (gpu 460 W, cpu 118 W)", "GBP 3.48/day at current draw  (gpu+cpu, 30.31p/kWh)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, gone := range []string{"since start", "average draw", "per 1M tokens"} {
		if strings.Contains(out, gone) {
			t.Errorf("one-shot output has %q:\n%s", gone, out)
		}
	}
}

func TestFmtCount(t *testing.T) {
	for in, want := range map[int]string{0: "0", 9999: "9999", 10000: "10.0k", 262144: "262.1k", 1500000: "1.5M"} {
		if got := fmtCount(in); got != want {
			t.Errorf("%d: %q want %q", in, got, want)
		}
	}
}

func TestRenderPlainPowerBreakdown(t *testing.T) {
	s := collect.Snapshot{Reachable: true, HasPower: true, PowerW: 412, EnergyWh: 420, JoulesPerToken: 10.3,
		HasGPUPower: true, GPUPowerW: 285, HasCPUPower: true, CPUPowerW: 127}
	out := RenderPlain(s)
	for _, want := range []string{"power 412 W (gpu 285 W, cpu 127 W)", "energy 420 Wh since start", "10.3 J/token"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	s.HasCPUPower, s.CPUPowerW, s.PowerW = false, 0, 285
	out = RenderPlain(s)
	if strings.Contains(out, "cpu") || strings.Contains(out, "(") || !strings.Contains(out, "power 285 W") {
		t.Errorf("gpu only:\n%s", out)
	}
	s.HasGPUPower, s.GPUPowerW, s.HasCPUPower, s.CPUPowerW, s.PowerW = false, 0, true, 127, 127
	out = RenderPlain(s)
	if strings.Contains(out, "(") || !strings.Contains(out, "power 127 W") {
		t.Errorf("cpu only:\n%s", out)
	}
	// a GPU that reports 0 W is still GPU power: the split follows HasGPUPower
	s.HasGPUPower = true
	if out = RenderPlain(s); !strings.Contains(out, "power 127 W (gpu 0 W, cpu 127 W)") {
		t.Errorf("gpu at 0 W:\n%s", out)
	}
}

// An idle slot's counters belong to its last task: --once prints only "idle".
func TestRenderPlainIdleSlot(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "b12679", "slots_idle.json"))
	if err != nil {
		t.Fatal(err)
	}
	slots, err := llama.ParseSlots(b)
	if err != nil || len(slots) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	views, _, _ := stats.NewTracker().Update(t0, slots)
	out := RenderPlain(collect.Snapshot{Reachable: true, Slots: views})
	for _, v := range views {
		want := fmt.Sprintf("slot %d  idle\n", v.ID)
		if v.State != stats.Idle || !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "slot ") && !strings.HasSuffix(l, "  idle") {
			t.Errorf("idle slot line shows its last task: %q", l)
		}
	}
}

// promptSlot is a generating slot where n_prompt_tokens includes the generated
// tokens (as on b12679): the prompt is cache + processed.
func promptSlot() stats.SlotView {
	return stats.SlotView{Slot: llama.Slot{ID: 0, NCtx: 262144, IsProcessing: true, TaskID: 1,
		PromptTokens: 138291, PromptCache: 131727, PromptProcessed: 2040, Decoded: 4522},
		State: stats.Generating, CtxUsed: 138291, CacheShare: 131727.0 / 133767}
}

func TestPromptColumn(t *testing.T) {
	v := promptSlot()
	out := RenderPlain(collect.Snapshot{Reachable: true, Slots: []stats.SlotView{v}})
	if !strings.Contains(out, "ctx 138.3k/262.1k") || !strings.Contains(out, "prompt 133.8k (cached 98%)") {
		t.Errorf("plain:\n%s", out)
	}
	s := idleSnapshot()
	s.Slots = []stats.SlotView{v}
	frame := stripANSI(Render(s, opts(135, 40, s)))
	if !strings.Contains(frame, "133.8k (98% cached)") || !strings.Contains(frame, "138.3k") {
		t.Errorf("frame:\n%s", frame)
	}
	// both counters absent: fall back to n_prompt_tokens
	v.PromptCache, v.PromptProcessed = 0, 0
	if out := RenderPlain(collect.Snapshot{Reachable: true, Slots: []stats.SlotView{v}}); !strings.Contains(out, "prompt 138.3k") {
		t.Errorf("fallback:\n%s", out)
	}
}

func TestRenderPlainNewLines(t *testing.T) {
	s := fullSnapshot(t)
	s.DBStatus = "off (locked)"
	out := RenderPlain(s)
	for _, want := range []string{
		"recent 12 finished, latest " + s.Recent[0].At.Local().Format("15:04:05") + " slot 0  prompt 131.7k (97% cached)  generated 300  4.4s  83.4 tok/s  ttft 0.8s",
		"host cpu 3% of 64 cores  load 1.14 1.15 1.10  ram 18.6/251.5 GB  swap 0.0/1.0 GB  disk / 1.5T free of 1.8T",
		"process pid 115429  cpu 105% (of one core)  mem 11.7 GB  threads 103",
		"container llama-server-main  running  unhealthy  up 3h12m  restarts 0",
		"cost 46p since start  GBP 3.48/day at average draw  47p per 1M tokens  (gpu+cpu, 30.31p/kWh)",
		"today 11.5 kWh  GBP 3.49  1.2M gen tokens  42 requests",
		"month 304 kWh  GBP 92.14  31.4M gen tokens  1093 requests",
		"all time 871 kWh  GBP 264.00  88.7M gen tokens  3120 requests",
		"db: off (locked)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	assertPlain(t, out)
	s.Recent, s.Host, s.Proc, s.Container = nil, nil, nil, nil
	s.Cost, s.Today, s.Month, s.AllTime = collect.Cost{}, store.Totals{}, store.Totals{}, store.Totals{}
	out = RenderPlain(s)
	for _, gone := range []string{"recent", "host", "process", "container", "cost", "today"} {
		if strings.Contains(out, gone+" ") {
			t.Errorf("%q without data:\n%s", gone, out)
		}
	}
}

func TestRenderPlainStatus(t *testing.T) {
	s := idleSnapshot()
	if out := RenderPlain(s); !strings.Contains(out, "\nstatus ok\n") {
		t.Errorf("no status ok line:\n%s", out)
	}
	s.Reachable = false
	if out := RenderPlain(s); !strings.Contains(out, "\nstatus bad\n") {
		t.Errorf("no status bad line:\n%s", out)
	}
}
