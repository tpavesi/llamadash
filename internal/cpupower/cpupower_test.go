package cpupower

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// ---- pure logic: runs on every OS ----

func TestIsPackageZone(t *testing.T) {
	for _, tc := range []struct {
		dir, name string
		want      bool
	}{
		{"intel-rapl:0", "package-0\n", true},
		{"intel-rapl:12", "package-1", true},
		{"intel-rapl:0", "  package-0 \n", true},
		{"intel-rapl:0", "psys\n", false},
		{"intel-rapl:0:0", "package-0\n", false},
		{"intel-rapl:0:0", "core\n", false},
		{"intel-rapl-mmio:0", "package-0\n", false},
		{"intel-rapl:", "package-0\n", false},
		{"intel-rapl:x", "package-0\n", false},
		{"intel-rapl", "package-0\n", false},
		{"", "", false},
		{"intel-rapl:0", "", false},
	} {
		if got := isPackageZone(tc.dir, tc.name); got != tc.want {
			t.Errorf("isPackageZone(%q,%q)=%v want %v", tc.dir, tc.name, got, tc.want)
		}
	}
}

func TestZoneDelta(t *testing.T) {
	for _, tc := range []struct {
		name          string
		prev, cur, mx uint64
		want          uint64
		ok            bool
	}{
		{"forward", 1_000_000, 61_000_000, 0, 60_000_000, true},
		{"equal", 5, 5, 0, 0, true},
		{"wrap", 262143000000, 1000000, 262143328850, 1328850, true},
		{"decrease without max is a reset", 100, 50, 0, 0, false},
		{"wrap larger than max is bogus", 100, 50, 10, 0, false},
	} {
		got, ok := zoneDelta(tc.prev, tc.cur, tc.mx)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d,%v) want (%d,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestClassifyGap(t *testing.T) {
	for _, tc := range []struct {
		dt   time.Duration
		want gap
	}{
		{-time.Second, gapReplace}, {0, gapKeep}, {time.Millisecond, gapKeep}, {49 * time.Millisecond, gapKeep},
		{50 * time.Millisecond, gapOK}, {2 * time.Second, gapOK}, {10 * time.Second, gapOK},
		{30 * time.Second, gapOK}, {30*time.Second + time.Millisecond, gapReplace}, {60 * time.Second, gapReplace},
	} {
		if got := classifyGap(tc.dt); got != tc.want {
			t.Errorf("classifyGap(%v)=%v want %v", tc.dt, got, tc.want)
		}
	}
}

// At --interval 10s the real gap is the interval plus the poll time, about 10.05s.
func TestClassifyGapTenSecondCadence(t *testing.T) {
	if got := classifyGap(10050 * time.Millisecond); got != gapOK {
		t.Fatalf("classifyGap(10.05s)=%v want gapOK", got)
	}
}

func TestSumDelta(t *testing.T) {
	prev := map[string]counter{"a": {1_000_000, 0}, "b": {0, 0}}
	cur := map[string]counter{"a": {61_000_000, 0}, "b": {40_000_000, 0}}
	j, ok := sumDelta(prev, cur)
	if !ok || math.Abs(j-100) > 1e-9 {
		t.Fatalf("j=%v ok=%v", j, ok)
	}
	// a zone that appeared only now contributes nothing but does not break the rest
	cur["c"] = counter{5, 0}
	if j, ok = sumDelta(prev, cur); !ok || math.Abs(j-100) > 1e-9 {
		t.Fatalf("new zone: j=%v ok=%v", j, ok)
	}
	// any zone reset invalidates the interval
	cur["b"] = counter{0, 0}
	prev["b"] = counter{9, 0}
	if _, ok = sumDelta(prev, cur); ok {
		t.Fatal("reset must not be ok")
	}
	// no overlapping zones
	if _, ok = sumDelta(prev, map[string]counter{"z": {1, 0}}); ok {
		t.Fatal("no common zones must not be ok")
	}
	if _, ok = sumDelta(nil, nil); ok {
		t.Fatal("empty must not be ok")
	}
}

// The user's machine: one intel-rapl:0 package-0 zone with an intel-rapl:0:0
// core sub-zone, counter range 65532610987 uJ.
const realMax = 65532610987

func TestRealShapePure(t *testing.T) {
	if !isPackageZone("intel-rapl:0", "package-0\n") || isPackageZone("intel-rapl:0:0", "core\n") {
		t.Fatal("zone selection")
	}
	prev := map[string]counter{"intel-rapl:0": {realMax - 2_000_000, realMax}}
	cur := map[string]counter{"intel-rapl:0": {3_000_000, realMax}} // wrapped: 5 J used
	j, ok := sumDelta(prev, cur)
	if !ok || math.Abs(j-5) > 1e-9 {
		t.Fatalf("wrap across %d: j=%v ok=%v", uint64(realMax), j, ok)
	}
}

func TestNewReaderDefaultRoot(t *testing.T) {
	if NewReader("").root != "/sys/class/powercap" {
		t.Fatal("default root")
	}
}

// ---- filesystem layer: needs ':' in directory names, so not on Windows ----

type zone struct {
	dir, name string
	energy    string
	max       string // "" -> file absent
}

func tree(t *testing.T, zs ...zone) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sysfs fake tree needs ':' in directory names")
	}
	root := t.TempDir()
	for _, z := range zs {
		d := filepath.Join(root, z.dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		put(t, d, "name", z.name+"\n")
		put(t, d, "energy_uj", z.energy+"\n")
		if z.max != "" {
			put(t, d, "max_energy_range_uj", z.max+"\n")
		}
	}
	return root
}

func put(t *testing.T, dir, file, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setEnergy(t *testing.T, root, dir string, uj uint64) {
	t.Helper()
	put(t, filepath.Join(root, dir), "energy_uj", strconv.FormatUint(uj, 10)+"\n")
}

func TestReadTwoPackages(t *testing.T) {
	root := tree(t,
		zone{"intel-rapl:0", "package-0", "1000000", "262143328850"},
		zone{"intel-rapl:1", "package-1", "0", "262143328850"})
	r := NewReader(root)
	if _, _, ok, err := r.Read(t0); ok || err != nil {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	setEnergy(t, root, "intel-rapl:0", 61_000_000)
	setEnergy(t, root, "intel-rapl:1", 40_000_000)
	w, j, ok, err := r.Read(t0.Add(2 * time.Second))
	if err != nil || !ok || math.Abs(j-100) > 1e-9 || math.Abs(w-50) > 1e-9 {
		t.Fatalf("w=%v j=%v ok=%v err=%v", w, j, ok, err)
	}
}

func TestFirstReadNotOK(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "5", "100"})
	if _, _, ok, err := NewReader(root).Read(t0); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestIgnoresNonPackageZones(t *testing.T) {
	root := tree(t,
		zone{"intel-rapl:0", "package-0", "0", "1000000000"},
		zone{"intel-rapl:1", "psys", "0", "1000000000"},
		zone{"intel-rapl:0:0", "core", "0", "1000000000"},
		zone{"intel-rapl-mmio:0", "package-0", "0", "1000000000"})
	r := NewReader(root)
	r.Read(t0)
	for _, d := range []string{"intel-rapl:0", "intel-rapl:1", "intel-rapl:0:0", "intel-rapl-mmio:0"} {
		setEnergy(t, root, d, 10_000_000)
	}
	_, j, ok, err := r.Read(t0.Add(time.Second))
	if err != nil || !ok || math.Abs(j-10) > 1e-9 {
		t.Fatalf("j=%v ok=%v err=%v", j, ok, err)
	}
}

func TestWraparound(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "262143000000", "262143328850"})
	r := NewReader(root)
	r.Read(t0)
	setEnergy(t, root, "intel-rapl:0", 1_000_000)
	w, j, ok, err := r.Read(t0.Add(time.Second))
	if err != nil || !ok || j <= 0 || math.Abs(j-1.32885) > 1e-6 || math.Abs(w-j) > 1e-9 {
		t.Fatalf("w=%v j=%v ok=%v err=%v", w, j, ok, err)
	}
}

func TestRealShapeFilesystem(t *testing.T) {
	root := tree(t,
		zone{"intel-rapl:0", "package-0", strconv.FormatUint(realMax-2_000_000, 10), strconv.FormatUint(realMax, 10)},
		zone{"intel-rapl:0:0", "core", "1000", strconv.FormatUint(realMax, 10)})
	r := NewReader(root)
	if _, _, ok, err := r.Read(t0); ok || err != nil {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	setEnergy(t, root, "intel-rapl:0", 3_000_000)           // wrapped: 5 J
	setEnergy(t, root, "intel-rapl:0:0", 4_000_000_000_000) // the core sub-zone is not counted
	w, j, ok, err := r.Read(t0.Add(2 * time.Second))
	if err != nil || !ok || math.Abs(j-5) > 1e-9 || math.Abs(w-2.5) > 1e-9 {
		t.Fatalf("w=%v j=%v ok=%v err=%v", w, j, ok, err)
	}
}

func TestDecreaseWithoutMaxIsReset(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "5000000", ""})
	r := NewReader(root)
	r.Read(t0)
	setEnergy(t, root, "intel-rapl:0", 1_000_000)
	if _, _, ok, err := r.Read(t0.Add(time.Second)); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	setEnergy(t, root, "intel-rapl:0", 3_000_000) // baseline was replaced by 1_000_000
	_, j, ok, _ := r.Read(t0.Add(2 * time.Second))
	if !ok || math.Abs(j-2) > 1e-9 {
		t.Fatalf("j=%v ok=%v", j, ok)
	}
}

func TestLongGapResets(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "0", "1000000000"})
	r := NewReader(root)
	r.Read(t0)
	setEnergy(t, root, "intel-rapl:0", 600_000_000)
	if _, _, ok, err := r.Read(t0.Add(60 * time.Second)); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	setEnergy(t, root, "intel-rapl:0", 610_000_000)
	w, _, ok, _ := r.Read(t0.Add(61 * time.Second))
	if !ok || math.Abs(w-10) > 1e-9 {
		t.Fatalf("w=%v ok=%v", w, ok)
	}
}

func TestReadTenSecondCadence(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "0", "65532610987"})
	r := NewReader(root)
	r.Read(t0)
	const dt = 10050 * time.Millisecond
	setEnergy(t, root, "intel-rapl:0", 1_005_000_000) // 100 W for 10.05s
	w, j, ok, err := r.Read(t0.Add(dt))
	if err != nil || !ok || math.Abs(j-1005) > 1e-6 || math.Abs(w-100) > 1e-6 {
		t.Fatalf("w=%v j=%v ok=%v err=%v", w, j, ok, err)
	}
}

func TestTinyGap(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "0", "1000000000"})
	r := NewReader(root)
	r.Read(t0)
	setEnergy(t, root, "intel-rapl:0", 1_000_000)
	if _, _, ok, err := r.Read(t0.Add(time.Millisecond)); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// baseline kept: the energy accrued so far counts in the next valid interval
	w, j, ok, _ := r.Read(t0.Add(time.Second))
	if !ok || math.Abs(j-1) > 1e-9 || math.Abs(w-1) > 1e-9 {
		t.Fatalf("w=%v j=%v ok=%v", w, j, ok)
	}
}

func TestUnavailableNoZones(t *testing.T) { // runs on every OS
	if _, _, _, err := NewReader(t.TempDir()).Read(t0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty root: %v", err)
	}
	if _, _, _, err := NewReader(filepath.Join(t.TempDir(), "nope")).Read(t0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing root: %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	root := tree(t, zone{"intel-rapl:0", "package-0", "0", "100"})
	p := filepath.Join(root, "intel-rapl:0", "energy_uj")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := NewReader(root).Read(t0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable energy_uj: %v", err)
	}
}

func TestGarbageCounter(t *testing.T) {
	root := tree(t,
		zone{"intel-rapl:0", "package-0", "abc", "100"},
		zone{"intel-rapl:1", "package-1", "0", "1000000000"})
	r := NewReader(root)
	if _, _, ok, err := r.Read(t0); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	setEnergy(t, root, "intel-rapl:1", 5_000_000)
	_, j, ok, err := r.Read(t0.Add(time.Second))
	if err != nil || !ok || math.Abs(j-5) > 1e-9 {
		t.Fatalf("j=%v ok=%v err=%v", j, ok, err)
	}
	// all zones garbage: no panic, not ok
	put(t, filepath.Join(root, "intel-rapl:1"), "energy_uj", "-7\n")
	if _, _, ok, _ = r.Read(t0.Add(2 * time.Second)); ok {
		t.Fatal("all-garbage must not be ok")
	}
}
