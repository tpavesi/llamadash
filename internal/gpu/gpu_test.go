package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "nvidia-smi", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseOne(t *testing.T) {
	gs, err := Parse(fixture(t, "one_gpu.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 {
		t.Fatalf("got %d gpus", len(gs))
	}
	g := gs[0]
	if g.Index != 0 || g.Name != "NVIDIA GeForce RTX 3090" || g.UtilPct != 97 ||
		g.MemUsedMiB != 18742 || g.MemTotalMiB != 24576 || g.TempC != 71 || g.PowerW != 285.31 {
		t.Errorf("bad gpu: %+v", g)
	}
	if !g.HasUtil || !g.HasTemp || !g.HasPower {
		t.Errorf("Has flags: %+v", g)
	}
}

func TestParseTwo(t *testing.T) {
	gs, err := Parse(fixture(t, "two_gpus.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 || gs[0].Index != 0 || gs[1].Index != 1 {
		t.Fatalf("got %+v", gs)
	}
}

func TestParseNA(t *testing.T) {
	gs, err := Parse(fixture(t, "na_fields.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 {
		t.Fatalf("got %d gpus", len(gs))
	}
	g := gs[0]
	if g.HasPower || g.HasUtil || !g.HasTemp {
		t.Errorf("Has flags: %+v", g)
	}
	if g.MemUsedMiB != 18742 || g.MemTotalMiB != 24576 || g.TempC != 71 {
		t.Errorf("fields: %+v", g)
	}
}

func TestParseGarbage(t *testing.T) {
	for _, in := range []string{
		"",
		"NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.",
		"0, GPU, 5",
		"x, GPU, 1, 2, 3, 4, 5",
	} {
		gs, _ := Parse([]byte(in))
		if len(gs) != 0 {
			t.Errorf("%q: got %+v", in, gs)
		}
	}
}

func TestQueryMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Query(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestParseDecimalTemp(t *testing.T) {
	gs, err := Parse(fixture(t, "rtx5090_float_temp.csv"))
	if err != nil || len(gs) != 1 {
		t.Fatalf("got %+v, %v", gs, err)
	}
	g := gs[0]
	if g.Name != "NVIDIA GeForce RTX 5090" || g.TempC != 66.33 || g.PowerW != 475.80 || g.MemTotalMiB != 32607 ||
		g.MemUsedMiB != 29965 || g.UtilPct != 92 || !g.HasTemp || !g.HasPower || !g.HasUtil {
		t.Errorf("bad gpu: %+v", g)
	}
}

func TestNumRejectsNaNAndInf(t *testing.T) {
	for _, in := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", " -inf "} {
		if v, ok := num(in); ok {
			t.Errorf("num(%q) = %v, ok", in, v)
		}
	}
	if v, ok := num(" 66.33 "); !ok || v != 66.33 {
		t.Errorf("num(66.33) = %v, %v", v, ok)
	}
}

// A hung nvidia-smi (here a shell script whose child keeps stdout open) must
// return promptly with the context's error.
func TestQueryHonoursContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake nvidia-smi is a shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\necho '0, fake, 1, 2, 3, 4, 5'\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Query(ctx)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Query returned after %v", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline error", err)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := Query(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
}

func TestParseEightColumns(t *testing.T) {
	gs, err := Parse(fixture(t, "rtx5090_fan.csv"))
	if err != nil || len(gs) != 1 {
		t.Fatalf("got %+v, %v", gs, err)
	}
	g := gs[0]
	if g.TempC != 64.37 || g.PowerW != 460.46 || g.UtilPct != 93 || g.FanPct != 33 || !g.HasFan {
		t.Errorf("bad gpu: %+v", g)
	}
	gs, err = Parse([]byte("0, GPU, 5, 1, 2, 60, 100, [N/A]\n1, GPU, 5, 1, 2, 60, 100, [Not Supported]\n"))
	if err != nil || len(gs) != 2 || gs[0].HasFan || gs[1].HasFan || gs[0].FanPct != 0 || !gs[0].HasPower {
		t.Errorf("N/A fan: %+v, %v", gs, err)
	}
	gs, err = Parse([]byte("0, GPU, 5, 1, 2, 60, 100\n"))
	if err != nil || len(gs) != 1 || gs[0].HasFan {
		t.Errorf("7 columns: %+v, %v", gs, err)
	}
	if gs, _ := Parse([]byte("0, GPU, 5, 1, 2, 60, 100, 5, 6\n")); len(gs) != 0 {
		t.Errorf("9 columns accepted: %+v", gs)
	}
}
