package ui

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llamadash/internal/collect"
	"llamadash/internal/container"
)

// TestWriteScreenshots writes the README's frames (with colour escapes) into the
// directory named by LLAMADASH_SHOTS; tools/screenshots.py turns them into images.
// The data is the synthetic snapshot the other tests use, with demo names.
func TestWriteScreenshots(t *testing.T) {
	dir := os.Getenv("LLAMADASH_SHOTS")
	if dir == "" {
		t.Skip("LLAMADASH_SHOTS not set")
	}
	demo := func() collect.Snapshot {
		s := fullSnapshot(t)
		p := *s.Props
		p.ModelAlias = "qwen3-27b"
		s.Props = &p
		c := *s.Container
		c.Name, c.Health = "llama-server", "healthy"
		s.Container = &c
		s.PSUWatts = 1000
		run := func(n, health string) container.Entry {
			return container.Entry{Name: n, State: "running", Health: health}
		}
		llama := run("llama-server", "healthy")
		llama.Llama = true
		s.Containers, s.ContainersAt = []container.Entry{
			llama, run("open-webui", "healthy"), run("postgres", ""), run("caddy", ""), run("searxng", ""),
			run("embeddings", "starting"),
			{Name: "backup-job", State: "exited", ExitCode: 1, HasExit: true},
			{Name: "tts", State: "exited", ExitCode: 0, HasExit: true},
			{Name: "comfyui", State: "exited", ExitCode: 137, HasExit: true},
		}, s.At.Add(-4*time.Second)
		return s
	}
	long := func(s collect.Snapshot, o Options) map[string][]float64 {
		out := map[string][]float64{}
		for key, n := range chartPlotWidths(s, o) {
			vals := make([]float64, n)
			for i := range vals {
				x := float64(i) / float64(n)
				// a working day: quiet night, busy afternoon, a short outage
				load := math.Max(0, math.Sin((x-0.25)*math.Pi*1.4)) * (0.75 + 0.25*math.Sin(float64(i)*1.7))
				switch key {
				case "gen_rate":
					vals[i] = 68 * load
				case "prompt_rate":
					vals[i] = 900 * load * (0.5 + 0.5*math.Abs(math.Sin(float64(i)*0.9)))
				case "power":
					vals[i] = 95 + 480*load
				case "gpu_util":
					vals[i] = 96 * load
				case "gpu_temp":
					vals[i] = 34 + 38*load
				case "gpu_mem_pct":
					vals[i] = 92
				case "host_cpu":
					vals[i] = 2 + 9*load
				}
				if x > 0.52 && x < 0.56 {
					vals[i] = math.NaN()
				}
			}
			out[key] = vals
		}
		return out
	}
	shots := []struct {
		name string
		w, h int
		rng  Range
	}{
		{"portrait-135x120", 135, 120, Range5m},
		{"landscape-160x62", 160, 62, Range5m},
		{"history-24h-135x64", 135, 64, Range24h},
		{"small-80x24", 80, 24, Range5m},
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sh := range shots {
		s := demo()
		// the tops a few days of use would have learned, the power chart's from the PSU
		o := Options{Width: sh.w, Height: sh.h, Interval: time.Second, Now: s.At, History: true, Range: sh.rng,
			Scales: Scales{PowerW: 1000, GenTPS: 92, PromptTPS: 1460}}
		if sh.rng.long() {
			o.LongAt, o.Long = s.At, long(s, o)
		}
		if err := os.WriteFile(filepath.Join(dir, sh.name+".ans"), []byte(Render(s, o)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
