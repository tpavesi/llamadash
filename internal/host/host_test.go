package host

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const dataDir = "../../testdata/proc"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// put writes content to <root>/<name>.
func put(t *testing.T, root, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeRoot is a proc root holding the fixture files as stat, meminfo, loadavg.
func fakeRoot(t *testing.T, stat string) string {
	t.Helper()
	root := t.TempDir()
	put(t, root, "stat", fixture(t, stat))
	put(t, root, "meminfo", fixture(t, "meminfo.txt"))
	put(t, root, "loadavg", fixture(t, "loadavg.txt"))
	return root
}

// stat_a -> stat_b (two seconds apart), aggregate "cpu" line, first 8 counters:
//
//	total  a = 1814586+91+329699+92654900+15440+0+25025+0 = 94839741
//	       b = 1814811+91+329706+92667488+15441+0+25025+0 = 94852562   dTotal = 12821
//	idle   a = 92654900+15440 = 92670340
//	       b = 92667488+15441 = 92682929                                dIdle  = 12589
//	CPU % = 100 * (1 - 12589/12821) = 1.80953...
const wantCPUPct = 1.8095312378129669

func TestCPUPctFromFixtures(t *testing.T) {
	root := fakeRoot(t, "stat_a.txt")
	r := NewReader(root)
	if _, err := r.Read(); err != nil {
		t.Fatal(err)
	}
	put(t, root, "stat", fixture(t, "stat_b.txt"))
	in, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !in.HasCPU || in.CPUPct < 0 || in.CPUPct > 100 || math.Abs(in.CPUPct-wantCPUPct) > 1e-9 {
		t.Errorf("CPUPct = %v (HasCPU %v), want %v", in.CPUPct, in.HasCPU, wantCPUPct)
	}
	if in.Cores != 64 {
		t.Errorf("Cores = %d, want 64", in.Cores)
	}
}

func TestFirstReadNoCPU(t *testing.T) {
	in, err := NewReader(fakeRoot(t, "stat_a.txt")).Read()
	if err != nil {
		t.Fatal(err)
	}
	if in.HasCPU || in.CPUPct != 0 || in.Cores != 64 {
		t.Errorf("first read: %+v", in)
	}
}

func TestMemAndLoad(t *testing.T) {
	in, err := NewReader(fakeRoot(t, "stat_a.txt")).Read()
	if err != nil {
		t.Fatal(err)
	}
	if in.MemTotalKB != 263764736 || in.MemAvailKB != 244305928 || in.SwapTotalKB != 1000444 || in.SwapFreeKB != 1000444 {
		t.Errorf("memory: %+v", in)
	}
	if in.Load1 != 1.14 || in.Load5 != 1.15 || in.Load15 != 1.10 {
		t.Errorf("load: %v %v %v", in.Load1, in.Load5, in.Load15)
	}
}

func TestHostGarbage(t *testing.T) {
	for name, content := range map[string]string{
		"empty":     "",
		"truncated": "cpu  18145",
		"text":      "cpu  a b c d e\ncpu0 x y\nhello",
		"short":     "cpu  1 2 3\n",
	} {
		root := t.TempDir()
		put(t, root, "stat", []byte(content))
		put(t, root, "meminfo", []byte("MemTotal: lots kB\nSwapFree:\nMemAvailable"))
		put(t, root, "loadavg", []byte("NaN inf"))
		r := NewReader(root)
		for i := 0; i < 2; i++ {
			in, err := r.Read()
			if err != nil || in.HasCPU || in.MemTotalKB != 0 || in.Load1 != 0 || in.Load5 != 0 {
				t.Errorf("%s read %d: %+v, %v", name, i, in, err)
			}
		}
	}
	for _, root := range []string{filepath.Join(t.TempDir(), "nope"), t.TempDir()} {
		if _, err := NewReader(root).Read(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("root %q: err = %v, want ErrUnavailable", root, err)
		}
	}
}

func TestCPUCounterBackwards(t *testing.T) {
	root := fakeRoot(t, "stat_b.txt")
	r := NewReader(root)
	if _, err := r.Read(); err != nil {
		t.Fatal(err)
	}
	put(t, root, "stat", fixture(t, "stat_a.txt"))
	in, err := r.Read()
	if err != nil || in.HasCPU || in.CPUPct != 0 {
		t.Errorf("backwards: %+v, %v", in, err)
	}
	// the new counters are the baseline: a forward step works again
	put(t, root, "stat", fixture(t, "stat_b.txt"))
	if in, _ = r.Read(); !in.HasCPU || math.Abs(in.CPUPct-wantCPUPct) > 1e-9 {
		t.Errorf("recovery: %+v", in)
	}
}
