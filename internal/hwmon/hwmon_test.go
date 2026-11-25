package hwmon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dataDir = "../../testdata/hwmon"

// fanList renders fans as "chip/label=rpm" for compact comparison.
func fanList(fs []Fan) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, fmt.Sprintf("%s/%s=%d", f.Chip, f.Label, f.RPM))
	}
	return strings.Join(parts, " ")
}

func TestReadAMDBoard(t *testing.T) {
	r, err := NewReader(filepath.Join(dataDir, "amd")).Read()
	if err != nil {
		t.Fatal(err)
	}
	want := "nct6798/fan 1=1250 nct6798/fan 2=890 nct6798/fan 3=0 nct6798/fan 4=0 nct6798/fan 5=1020 " +
		"nct6798/fan 6=0 nct6798/fan 7=1460"
	if got := fanList(r.Fans); got != want {
		t.Errorf("fans\n got %s\nwant %s", got, want)
	}
	if !r.HasCPUTemp || r.CPUTempC != 60.125 {
		t.Errorf("cpu temp %v %v, want 60.125 (Tctl)", r.CPUTempC, r.HasCPUTemp)
	}
	keys := map[string]bool{}
	for _, f := range r.Fans {
		if f.Key == "" || keys[f.Key] {
			t.Errorf("fan key %q empty or repeated", f.Key)
		}
		keys[f.Key] = true
	}
}

func TestReadIntelBoard(t *testing.T) {
	r, err := NewReader(filepath.Join(dataDir, "intel")).Read()
	if err != nil {
		t.Fatal(err)
	}
	want := "it8688/CPU_FAN=1180 it8688/SYS_FAN1=760 it8688/fan 3=0 it8688/PUMP=2350"
	if got := fanList(r.Fans); got != want {
		t.Errorf("fans\n got %s\nwant %s", got, want)
	}
	if !r.HasCPUTemp || r.CPUTempC != 54 {
		t.Errorf("cpu temp %v %v, want 54 (Package id 0)", r.CPUTempC, r.HasCPUTemp)
	}
}

func TestReadGPUOnlyUnavailable(t *testing.T) {
	if _, err := NewReader(filepath.Join(dataDir, "gpuonly")).Read(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v, want ErrUnavailable", err)
	}
}

func TestReadMissingTree(t *testing.T) {
	for _, root := range []string{filepath.Join(t.TempDir(), "nope"), t.TempDir()} {
		if _, err := NewReader(root).Read(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err %v, want ErrUnavailable", root, err)
		}
	}
}

func TestReadGarbage(t *testing.T) {
	r, err := NewReader(filepath.Join(dataDir, "garbage")).Read()
	if err != nil {
		t.Fatal(err)
	}
	// empty, text, huge, negative, fractional and >100000 are skipped; no newline
	// and surrounding spaces are accepted; the label loses its control characters
	// and is capped at 24 bytes
	want := "nct6775/fan 5=1100 nct6775/fan 6=800 nct6775/[31mCHA_FAN with a very=700"
	if got := fanList(r.Fans); got != want {
		t.Errorf("fans\n got %q\nwant %q", got, want)
	}
	if r.HasCPUTemp {
		t.Errorf("Tctl of 200 C accepted: %v", r.CPUTempC)
	}
}

// put writes content to root/rel, creating directories.
func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadOrderNumeric(t *testing.T) {
	root := t.TempDir()
	put(t, root, "hwmon10/name", "it87\n")
	put(t, root, "hwmon10/fan1_input", "500\n")
	put(t, root, "hwmon9/name", "nct6775\n")
	for _, n := range []int{10, 2, 9, 1} {
		put(t, root, fmt.Sprintf("hwmon9/fan%d_input", n), fmt.Sprintf("%d\n", 1000+n))
	}
	put(t, root, "hwmon9/fanX_input", "1\n")
	put(t, root, "hwmon9/fan3_input_extra", "1\n")
	put(t, root, "other/fan1_input", "1\n")
	put(t, root, "hwmon11/fan1_input", "600\n") // no name: the directory stands in
	r, err := NewReader(root).Read()
	if err != nil {
		t.Fatal(err)
	}
	want := "nct6775/fan 1=1001 nct6775/fan 2=1002 nct6775/fan 9=1009 nct6775/fan 10=1010 it87/fan 1=500 hwmon11/fan 1=600"
	if got := fanList(r.Fans); got != want {
		t.Errorf("order\n got %s\nwant %s", got, want)
	}
}

func TestReadAtMost16Fans(t *testing.T) {
	root := t.TempDir()
	put(t, root, "hwmon0/name", "nct6775\n")
	for n := 1; n <= 20; n++ {
		put(t, root, fmt.Sprintf("hwmon0/fan%d_input", n), "900\n")
	}
	r, err := NewReader(root).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Fans) != 16 || r.Fans[15].Label != "fan 16" {
		t.Errorf("%d fans: %s", len(r.Fans), fanList(r.Fans))
	}
}

func TestCPUTempChoice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  float64
		ok    bool
	}{
		{"k10temp Tdie", map[string]string{"name": "k10temp", "temp1_input": "70000", "temp1_label": "Tctl2",
			"temp2_input": "61500", "temp2_label": "Tdie"}, 61.5, true},
		{"zenpower temp1", map[string]string{"name": "zenpower", "temp1_input": "55250"}, 55.25, true},
		{"coretemp hottest core", map[string]string{"name": "coretemp", "temp2_input": "48000", "temp2_label": "Core 0",
			"temp3_input": "51000", "temp3_label": "Core 1", "temp4_input": "47000", "temp4_label": "Core 2"}, 51, true},
		{"cpu_thermal", map[string]string{"name": "cpu_thermal", "temp1_input": "-12000"}, -12, true},
		{"too cold", map[string]string{"name": "cpu_thermal", "temp1_input": "-41000"}, 0, false},
		{"limits", map[string]string{"name": "cpu_thermal", "temp1_input": "150000"}, 150, true},
		{"junk", map[string]string{"name": "k10temp", "temp1_input": "hot", "temp1_label": "Tctl"}, 0, false},
		{"other chip", map[string]string{"name": "acpitz", "temp1_input": "40000"}, 0, false},
	} {
		root := t.TempDir()
		for f, c := range tc.files {
			put(t, root, "hwmon0/"+f, c+"\n")
		}
		put(t, root, "hwmon1/name", "nct6775\n")
		put(t, root, "hwmon1/fan1_input", "800\n") // so the read is never unavailable
		r, err := NewReader(root).Read()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.HasCPUTemp != tc.ok || r.CPUTempC != tc.want {
			t.Errorf("%s: %v %v, want %v %v", tc.name, r.CPUTempC, r.HasCPUTemp, tc.want, tc.ok)
		}
	}
}

func TestCPUTempOnlyIsAvailable(t *testing.T) {
	root := t.TempDir()
	put(t, root, "hwmon0/name", "coretemp\n")
	put(t, root, "hwmon0/temp1_input", "45000\n")
	put(t, root, "hwmon0/temp1_label", "Package id 0\n")
	r, err := NewReader(root).Read()
	if err != nil || !r.HasCPUTemp || r.CPUTempC != 45 || len(r.Fans) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestParseValue(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1250\n": 1250, " 7 \n": 7, "-3\n": -3} {
		if v, ok := parseValue(in); !ok || v != want {
			t.Errorf("%q: %d %v", in, v, ok)
		}
	}
	for _, in := range []string{"", "\n", "abc", "12.5", "1 2", "99999999999999999999999"} {
		if _, ok := parseValue(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

// A fan's identity is its chip's name and fan number, stable across boots
// (not the hwmonN directory); a second chip of the same name is name#2, odd
// characters become _.
func TestFanIdentity(t *testing.T) {
	root := t.TempDir()
	put(t, root, "hwmon3/name", "nct6683\n")
	put(t, root, "hwmon3/fan10_input", "900\n")
	put(t, root, "hwmon3/fan9_input", "800\n")
	put(t, root, "hwmon5/name", "coretemp\n") // no fans: does not count for the numbering
	put(t, root, "hwmon5/temp1_input", "40000\n")
	put(t, root, "hwmon7/name", "nct6683\n")
	put(t, root, "hwmon7/fan1_input", "700\n")
	put(t, root, "hwmon8/name", "nct6683\n")
	put(t, root, "hwmon8/fan2_input", "0\n")
	put(t, root, "hwmon9/name", "acpi fan:x/y\n")
	put(t, root, "hwmon9/fan1_input", "100\n")
	put(t, root, "hwmon12/name", "it87#weird\n")
	put(t, root, "hwmon12/fan3_input", "100\n")
	r, err := NewReader(root).Read()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range r.Fans {
		got = append(got, f.Key)
		if !ValidID(f.Key) {
			t.Errorf("%q not a valid identity", f.Key)
		}
	}
	want := "nct6683:fan9 nct6683:fan10 nct6683#2:fan1 nct6683#3:fan2 acpi_fan_x_y:fan1 it87_weird:fan3"
	if strings.Join(got, " ") != want {
		t.Errorf("identities\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if r.Fans[2].Chip != "nct6683" {
		t.Errorf("the shown chip name keeps its own name: %q", r.Fans[2].Chip)
	}
}

func TestValidID(t *testing.T) {
	for id, ok := range map[string]bool{"nct6683:fan1": true, "nct6683#2:fan12": true, "a.b-c_d:fan0": true,
		"": false, "nct6683": false, ":fan1": false, "x:fan": false, "x:fanA": false, "x y:fan1": false,
		"x:fan1:fan2": false, "x:fan1234567": false, "x/y:fan1": false, "x:FAN1": false} {
		if ValidID(id) != ok {
			t.Errorf("ValidID(%q) = %v", id, !ok)
		}
	}
	chip, n, ok := ParseID("nct6683#2:fan12")
	if !ok || chip != "nct6683#2" || n != 12 {
		t.Errorf("ParseID: %q %d %v", chip, n, ok)
	}
}
