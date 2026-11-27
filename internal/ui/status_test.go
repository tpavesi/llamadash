package ui

import (
	"strings"
	"testing"

	"llamadash/internal/gpu"
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
