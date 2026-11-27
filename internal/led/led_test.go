package led

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "openrgb", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseDevices(t *testing.T) {
	d := ParseDevices(read(t, "list_detailed.txt"))
	if len(d) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(d), d)
	}
	if d[0].Index != 0 || d[0].Name != "ASRock TRX40 Creator" || d[0].Type != "Motherboard" {
		t.Errorf("device = %+v", d[0])
	}
	for _, m := range []string{"Off", "Static", "Spectrum Cycle", "Rainbow", "Direct"} {
		if !slices.Contains(d[0].Modes, m) {
			t.Errorf("mode %q missing from %q", m, d[0].Modes)
		}
	}
	if len(d[0].Modes) != 16 {
		t.Errorf("got %d modes, want 16: %q", len(d[0].Modes), d[0].Modes)
	}
	// Windows line endings give the same result
	crlf := ParseDevices([]byte(strings.ReplaceAll(string(read(t, "list_detailed.txt")), "\n", "\r\n")))
	if len(crlf) != 1 || crlf[0].Type != "Motherboard" || !slices.Equal(crlf[0].Modes, d[0].Modes) {
		t.Errorf("with CRLF: %+v", crlf)
	}
}

func TestParseDevicesBroken(t *testing.T) {
	if d := ParseDevices(read(t, "list_empty.txt")); len(d) != 0 {
		t.Errorf("empty input: %+v", d)
	}
	if d := ParseDevices(nil); len(d) != 0 {
		t.Errorf("nil input: %+v", d)
	}
	if d := ParseDevices([]byte("Segmentation fault\n\x00\xff\xfe 0:\n:\n 3: indented\n")); len(d) != 0 {
		t.Errorf("junk: %+v", d)
	}
	d := ParseDevices(read(t, "list_garbage.txt"))
	if len(d) == 0 || len(d) > 32 {
		t.Fatalf("got %d devices, want 1..32", len(d))
	}
	for _, dev := range d {
		if len(dev.Name) > 64 || dev.Name == "" {
			t.Errorf("name of %d bytes: %q", len(dev.Name), dev.Name)
		}
		for _, s := range append([]string{dev.Name, dev.Type}, dev.Modes...) {
			if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Errorf("control character in %q", s)
			}
			if len(s) > 64 {
				t.Errorf("%d bytes: %q", len(s), s)
			}
		}
		if len(dev.Modes) > 64 {
			t.Errorf("device %d has %d modes", dev.Index, len(dev.Modes))
		}
		if dev.Index < 0 || strings.Contains(dev.Name, "no index") || strings.Contains(dev.Name, "negative") ||
			strings.Contains(dev.Name, "too big") {
			t.Errorf("device that should have been skipped: %+v", dev)
		}
	}
	if d[0].Index != 7 || d[0].Name != "Bad[31mName Board" || d[0].Type != "Motherboard" {
		t.Errorf("first device = %+v", d[0])
	}
}

func TestPick(t *testing.T) {
	devs := ParseDevices(read(t, "list_two_devices.txt"))
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devs), devs)
	}
	idx := func(ds []Device) (out []int) {
		for _, d := range ds {
			out = append(out, d.Index)
		}
		return out
	}
	for name, want := range map[string][]int{"": {0}, "keyboard": {1}, "KEYBOARD": {1}, "trx40": {0}, "nothing": nil, "e": {0, 1}} {
		if got := idx(Pick(devs, name)); !slices.Equal(got, want) {
			t.Errorf("Pick(%q) = %v, want %v", name, got, want)
		}
	}
	devs[0].Type = "MOTHERBOARD"
	if got := idx(Pick(devs, "")); !slices.Equal(got, []int{0}) {
		t.Errorf("type match is case-sensitive: %v", got)
	}
	if got := Pick(nil, ""); len(got) != 0 {
		t.Errorf("Pick(nil) = %v", got)
	}
}

func TestArgs(t *testing.T) {
	devs := ParseDevices(read(t, "list_two_devices.txt"))
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2", len(devs))
	}
	head := []string{"--noautoconnect", "--config", "/x"}
	for st, colour := range map[State]string{Green: "00FF00", Amber: "FF7E00", Red: "FF0000"} {
		want := append(slices.Clone(head), "-d", "0", "-m", "Static", "-c", colour)
		if got := Args(devs[:1], st, "/x"); !slices.Equal(got, want) {
			t.Errorf("state %d: %q, want %q", st, got, want)
		}
	}
	if got, want := Args(devs[:1], Off, "/x"), append(slices.Clone(head), "-d", "0", "-m", "Off"); !slices.Equal(got, want) {
		t.Errorf("off: %q, want %q", got, want)
	}
	// a device without an Off mode goes black instead
	want := append(slices.Clone(head), "-d", "1", "-m", "Static", "-c", "000000")
	if got := Args(devs[1:], Off, "/x"); !slices.Equal(got, want) {
		t.Errorf("off without an Off mode: %q, want %q", got, want)
	}
	want = append(slices.Clone(head), "-d", "0", "-m", "Static", "-c", "FF0000", "-d", "1", "-m", "Static", "-c", "FF0000")
	if got := Args(devs, Red, "/x"); !slices.Equal(got, want) {
		t.Errorf("two devices: %q, want %q", got, want)
	}
	if got := Args(nil, Red, "/x"); got != nil {
		t.Errorf("no devices: %q, want nil", got)
	}
	// a state out of range is treated as off, never as a colour
	if got := Args(devs[:1], State(9), "/x"); !slices.Equal(got, append(slices.Clone(head), "-d", "0", "-m", "Off")) {
		t.Errorf("unknown state: %q", got)
	}
	if got, want := ListArgs("/x"), []string{"--noautoconnect", "--config", "/x", "--list-detailed"}; !slices.Equal(got, want) {
		t.Errorf("ListArgs = %q, want %q", got, want)
	}
}
