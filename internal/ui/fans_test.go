package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"llamadash/internal/collect"
)

// hostRow returns the Host panel's content line containing marker, without
// escapes and borders ("" when none).
func hostRow(s collect.Snapshot, o Options, marker string) string {
	for _, l := range panelContent(lines(Render(s, o)), "Host") {
		if p := stripANSI(l); strings.Contains(p, marker) {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "│|"))
		}
	}
	return ""
}

// rawHostRow is hostRow with the escapes kept.
func rawHostRow(s collect.Snapshot, o Options, marker string) string {
	for _, l := range panelContent(lines(Render(s, o)), "Host") {
		if strings.Contains(stripANSI(l), marker) {
			return l
		}
	}
	return ""
}

func TestHostCPUTemp(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	if row := hostRow(s, o, "cores"); !strings.HasSuffix(row, "load 1.14 1.15 1.10   temp 60°C") {
		t.Errorf("cpu row %q", row)
	}
	as := opts(135, 120, s)
	as.ASCII = true
	if out := stripANSI(Render(s, as)); !strings.Contains(out, "load 1.14 1.15 1.10   temp 60C") {
		t.Errorf("ASCII temp:\n%s", out)
	}
	for _, tc := range []struct {
		c     float64
		shown string
		fg    int // 0: no warning colour
		mark  string
	}{{84.4, "84", 0, ""}, {84.6, "85", 221, "!"}, {94.4, "94", 221, "!"}, {95, "95", 203, "!!"}, {104, "104", 203, "!!"}} {
		s.CPUTempC = tc.c
		raw := rawHostRow(s, opts(135, 120, s), "cores")
		if tc.fg != 0 && !strings.Contains(raw, fgSGR(tc.fg)+tc.shown+"°C") {
			t.Errorf("%v: not coloured %d: %q", tc.c, tc.fg, raw)
		}
		if tc.fg == 0 && (strings.Contains(raw, fgSGR(221)) || strings.Contains(raw, fgSGR(203))) {
			t.Errorf("%v: warning colour: %q", tc.c, raw)
		}
		if strings.Contains(stripANSI(raw), "!") {
			t.Errorf("%v: marker in colour: %q", tc.c, raw)
		}
		nc := opts(135, 120, s)
		nc.NoColor = true
		if row := hostRow(s, nc, "cores"); !strings.HasSuffix(row, "temp "+tc.shown+"°C"+tc.mark) {
			t.Errorf("%v NoColor: %q", tc.c, row)
		}
	}
	s.HasCPUTemp = false
	if row := hostRow(s, o, "cores"); strings.Contains(row, "temp") {
		t.Errorf("temp without a reading: %q", row)
	}
}

func TestHostFansRow(t *testing.T) {
	s := fullSnapshot(t)
	o := opts(135, 120, s)
	c := panelContent(lines(stripANSI(Render(s, o))), "Host")
	if len(c) != 4 || !strings.Contains(c[2], "free of") {
		t.Fatalf("Host rows: %q", c)
	}
	want := "fans   fan 1 1250   fan 2 890   fan 5 1020   fan 7 1460   rpm"
	if row := hostRow(s, o, "fans"); row != want {
		t.Errorf("fans row\n got %q\nwant %q", row, want)
	}
	if !strings.Contains(c[3], "│ "+want) {
		t.Errorf("fans row not last or not at the label column: %q", c[3])
	}

	// a stopped fan goes first, red, "!!" without colour
	s.Fans = fansFixture()
	s.Fans[2].RPM, s.Fans[2].Stopped = 0, true
	if row := hostRow(s, o, "fans"); row != "fans   fan 5 0   fan 1 1250   fan 2 890   fan 7 1460   rpm" {
		t.Errorf("stopped: %q", row)
	}
	if raw := rawHostRow(s, o, "fans"); !strings.Contains(raw, fgSGR(203)+"fan 5 0") {
		t.Errorf("stopped fan not red: %q", raw)
	}
	for _, mode := range []Options{{NoColor: true}, {ASCII: true}} {
		m := opts(135, 120, s)
		m.NoColor, m.ASCII = mode.NoColor, mode.ASCII
		if out := stripANSI(Render(s, m)); !strings.Contains(out, "fans   fan 5 0!!   fan 1 1250") {
			t.Errorf("stopped without colour (%+v):\n%s", mode, out)
		}
	}

	// fans of two chips carry the chip name
	s.Fans = append(fansFixture()[:1], collect.Fan{Label: "CPU_FAN", Chip: "it8688", RPM: 1180})
	if row := hostRow(s, o, "fans"); row != "fans   nct6798 fan 1 1250   it8688 CPU_FAN 1180   rpm" {
		t.Errorf("two chips: %q", row)
	}

	// no fans: no row; the panel is one row shorter
	s.Fans = nil
	if c := panelContent(lines(stripANSI(Render(s, o))), "Host"); len(c) != 3 || hostRow(s, o, "fans") != "" {
		t.Errorf("rows without fans: %q", c)
	}
}

func TestHostFansOverflow(t *testing.T) {
	s := fullSnapshot(t)
	s.Fans = nil
	for n := 1; n <= 16; n++ {
		s.Fans = append(s.Fans, collect.Fan{Label: fmt.Sprintf("CHA_FAN%d", n), Chip: "nct6798", RPM: 1000 + n})
	}
	for _, w := range []int{135, 100, 80, 40, 20} {
		o := opts(w, 120, s)
		row := hostRow(s, o, "fans")
		if w <= 20 {
			continue // the panel may be gone or the row cut by the box
		}
		f := strings.Fields(row)
		if len(f) < 3 || f[0] != "fans" || f[len(f)-1] != "rpm" || !strings.HasPrefix(f[len(f)-2], "+") {
			t.Fatalf("%d: %q", w, row)
		}
		more, err := strconv.Atoi(f[len(f)-2][1:])
		if err != nil {
			t.Fatalf("%d: %q", w, row)
		}
		if shown := strings.Count(row, "CHA_FAN"); shown+more != 16 || shown == 0 && w >= 80 {
			t.Errorf("%d: %d shown + %d = %d: %q", w, shown, more, shown+more, row)
		}
	}
}

func TestHostFansStale(t *testing.T) {
	s := fullSnapshot(t)
	s.HwmonAt = s.At.Add(-5 * time.Second)
	raw := lines(Render(s, opts(135, 120, s)))
	plain := lines(stripANSI(strings.Join(raw, "\n")))
	if title := plain[titleLine(plain, "┌ Host")]; !strings.Contains(title, "stale, 5s ago") || !dimmed(panelContent(raw, "Host")) {
		t.Errorf("host not stale with old fan readings: %q", title)
	}
}

func TestHostFansNoEscapes(t *testing.T) {
	s := fullSnapshot(t)
	s.Fans[0].Label, s.Fans[0].Chip = "evil\x1b[31m\x07", "chip\x1b]0;x"
	s.Fans[1].Chip = "other"
	for _, mode := range []Options{{}, {Color16: true}, {NoColor: true}, {ASCII: true}} {
		o := opts(135, 120, s)
		o.Color16, o.NoColor, o.ASCII = mode.Color16, mode.NoColor, mode.ASCII
		out := Render(s, o)
		if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x07") || strings.Contains(out, "\x1b[31m\x07") {
			t.Fatal("raw escape from a fan label in the output")
		}
	}
	if out := RenderPlain(s); strings.ContainsAny(out, "\x1b\x07") {
		t.Fatal("raw escape in the plain output")
	}
}

func TestRenderPlainFans(t *testing.T) {
	s := fullSnapshot(t)
	out := RenderPlain(s)
	for _, want := range []string{
		"disk / 1.5T free of 1.8T  temp 60 C\n",
		"\nfans  fan 1 1250  fan 2 890  fan 5 1020  fan 7 1460 rpm\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	assertPlain(t, out)
	s.Fans[2].RPM, s.Fans[2].Stopped = 0, true
	if out := RenderPlain(s); !strings.Contains(out, "\nfans  fan 5 0 STOPPED  fan 1 1250  fan 2 890  fan 7 1460 rpm\n") {
		t.Errorf("stopped:\n%s", out)
	}
	s.Fans = append(fansFixture()[:1], collect.Fan{Label: "CPU_FAN", Chip: "it8688", RPM: 1180})
	if out := RenderPlain(s); !strings.Contains(out, "\nfans  nct6798 fan 1 1250  it8688 CPU_FAN 1180 rpm\n") {
		t.Errorf("two chips:\n%s", out)
	}
	s.Fans, s.HasCPUTemp = nil, false
	if out := RenderPlain(s); strings.Contains(out, "fans") || strings.Contains(out, "temp") {
		t.Errorf("fans or temp without data:\n%s", out)
	}
}

// A known fan whose sensor is gone shows "n/a" in the warning colour ("!"
// without colour), after the stopped fans and before the running ones.
func TestHostFansMissing(t *testing.T) {
	s := fullSnapshot(t)
	s.Fans = append(fansFixture(), collect.Fan{Label: "fan 3", Chip: "nct6798", Missing: true})
	s.Fans[2].RPM, s.Fans[2].Stopped = 0, true
	o := opts(135, 120, s)
	if row := hostRow(s, o, "fans"); row != "fans   fan 5 0   fan 3 n/a   fan 1 1250   fan 2 890   fan 7 1460   rpm" {
		t.Errorf("order: %q", row)
	}
	if raw := rawHostRow(s, o, "fans"); !strings.Contains(raw, fgSGR(221)+"fan 3 n/a") {
		t.Errorf("missing fan not amber: %q", raw)
	}
	for _, mode := range []Options{{NoColor: true}, {ASCII: true}, {Color16: true}} {
		m := opts(135, 120, s)
		m.NoColor, m.ASCII, m.Color16 = mode.NoColor, mode.ASCII, mode.Color16
		out := stripANSI(Render(s, m))
		want := "fans   fan 5 0   fan 3 n/a   fan 1"
		if m.NoColor || m.ASCII {
			want = "fans   fan 5 0!!   fan 3 n/a!   fan 1"
		}
		if !strings.Contains(out, want) {
			t.Errorf("%+v: no %q in\n%s", mode, want, out)
		}
		// the layout sweep with a stopped, a missing and running fans
		for _, w := range []int{20, 40, 80, 135, 300} {
			for _, h := range []int{8, 24, 60} {
				m.Width, m.Height = w, h
				checkFits(t, "fans mix", Render(s, m), m)
			}
		}
	}
	if out := RenderPlain(s); !strings.Contains(out, "\nfans  fan 5 0 STOPPED  fan 3 n/a  fan 1 1250  fan 2 890  fan 7 1460 rpm\n") {
		t.Errorf("plain:\n%s", out)
	}
	// only missing fans (no sensor driver at all): the row is still there
	s.Fans = []collect.Fan{{Label: "fan 1", Chip: "it87", Missing: true}, {Label: "fan 2", Chip: "it87", Missing: true}}
	if row := hostRow(s, o, "fans"); row != "fans   fan 1 n/a   fan 2 n/a   rpm" {
		t.Errorf("all missing: %q", row)
	}
}
