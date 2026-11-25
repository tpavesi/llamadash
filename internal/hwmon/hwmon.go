// Package hwmon reads motherboard fan speeds and the CPU temperature from the
// kernel's hardware-monitoring tree (/sys/class/hwmon/hwmonN).
package hwmon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fan is one fan input of a chip.
type Fan struct {
	Key   string // the identity "chip:fanM" (see ID): stable across boots
	Chip  string // the chip's name file (cleaned), else the directory name
	Label string // fanM_label (cleaned), else "fan M"
	RPM   int
}

// Reading is one pass over the tree.
type Reading struct {
	Fans       []Fan // at most MaxFans, chip directory then fan number order
	CPUTempC   float64
	HasCPUTemp bool
}

// ErrUnavailable means the tree is missing or holds no usable fan or CPU
// temperature (no driver for the board's sensor chip, or not Linux).
var ErrUnavailable = errors.New("hwmon: no fan or CPU temperature sensor")

const (
	defaultRoot = "/sys/class/hwmon"
	MaxFans     = 16
	maxRPM      = 100000
	maxString   = 24  // bytes kept of a label or chip name
	maxFile     = 128 // bytes read of any sysfs file
	minTempC    = -40
	maxTempC    = 150
)

// gpuChips are drivers whose fans the GPU panel shows already.
var gpuChips = map[string]bool{"nvidia": true, "amdgpu": true, "nouveau": true}

// cpuChips are drivers that report the CPU temperature.
var cpuChips = map[string]bool{"k10temp": true, "zenpower": true, "coretemp": true, "cpu_thermal": true}

// Reader reads one hwmon tree. It holds no state between reads.
type Reader struct{ root string }

// NewReader returns a Reader over root; "" means /sys/class/hwmon.
func NewReader(root string) *Reader {
	if root == "" {
		root = defaultRoot
	}
	return &Reader{root: root}
}

// clean drops control characters, caps s at maxString bytes on a rune boundary
// and trims surrounding spaces.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > maxString {
		n := maxString
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return strings.TrimSpace(s)
}

// parseValue parses a sysfs integer, with or without the trailing newline.
func parseValue(s string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v, err == nil
}

// index parses the number in prefix<N>suffix; ok is false for anything else.
func index(name, prefix, suffix string) (int, bool) {
	n, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return 0, false
	}
	if n, ok = strings.CutSuffix(n, suffix); !ok || n == "" || len(n) > 6 {
		return 0, false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(n)
	return v, err == nil
}

// readSmall reads at most maxFile bytes of a regular file.
func readSmall(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFile))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// chip is one hwmonN directory: its number, path and the sensor numbers found,
// and the chip part of its fans' identities.
type chip struct {
	n          int
	dir, name  string
	fans, temp []int
	idName     string
}

// maxIDLen bounds an identity: a cleaned chip name, "#N", ":fan" and the number.
const maxIDLen = 48

// idChars are the characters of an identity's chip part ("#" only as the
// duplicate-name suffix the reader adds, but accepted anywhere when read back).
func idChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' || r == '#'
}

// idName is a chip name as the chip part of an identity: every character other
// than letters, digits, _ . - becomes _ (also #, which marks duplicates).
func idName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '#' || !idChar(r) {
			return '_'
		}
		return r
	}, name)
}

// ID is the identity of fan n of a chip whose identity part is chip: "chip:fanN".
func ID(chip string, n int) string { return chip + ":fan" + strconv.Itoa(n) }

// ParseID splits an identity into its chip part and fan number.
func ParseID(id string) (chip string, n int, ok bool) {
	if len(id) > maxIDLen {
		return "", 0, false
	}
	chip, num, found := strings.Cut(id, ":")
	if !found || chip == "" || strings.IndexFunc(chip, func(r rune) bool { return !idChar(r) }) >= 0 {
		return "", 0, false
	}
	n, ok = index(num, "fan", "")
	return chip, n, ok
}

// ValidID reports whether id is a fan identity: [A-Za-z0-9_.#-]+ then ":fanN".
func ValidID(id string) bool { _, _, ok := ParseID(id); return ok }

// listChips returns the hwmonN directories in numeric order; ok is false when
// the root cannot be listed.
func (r *Reader) listChips() ([]chip, bool) {
	ents, err := os.ReadDir(r.root)
	if err != nil {
		return nil, false
	}
	var out []chip
	for _, e := range ents {
		if n, ok := index(e.Name(), "hwmon", ""); ok {
			out = append(out, chip{n: n, dir: filepath.Join(r.root, e.Name())})
		}
	}
	slices.SortFunc(out, func(a, b chip) int { return a.n - b.n })
	return out, true
}

// scan fills the chip's name and its fan and temperature input numbers.
func (c *chip) scan() {
	c.name = filepath.Base(c.dir)
	if s, ok := readSmall(filepath.Join(c.dir, "name")); ok {
		if n := clean(s); n != "" {
			c.name = n
		}
	}
	ents, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if n, ok := index(e.Name(), "fan", "_input"); ok {
			c.fans = append(c.fans, n)
		} else if n, ok := index(e.Name(), "temp", "_input"); ok {
			c.temp = append(c.temp, n)
		}
	}
	slices.Sort(c.fans)
	slices.Sort(c.temp)
}

func (c *chip) file(kind string, n int, suffix string) string {
	return filepath.Join(c.dir, kind+strconv.Itoa(n)+suffix)
}

// fanReadings appends the chip's usable fans to out, up to MaxFans.
func (c *chip) fanReadings(out []Fan) []Fan {
	for _, n := range c.fans {
		if len(out) >= MaxFans {
			break
		}
		s, ok := readSmall(c.file("fan", n, "_input"))
		if !ok {
			continue
		}
		v, ok := parseValue(s)
		if !ok || v < 0 || v > maxRPM {
			continue
		}
		label := "fan " + strconv.Itoa(n)
		if l, ok := readSmall(c.file("fan", n, "_label")); ok {
			if l = clean(l); l != "" {
				label = l
			}
		}
		out = append(out, Fan{Key: ID(c.idName, n), Chip: c.name, Label: label, RPM: int(v)})
	}
	return out
}

// temp is one temperature input: its label and millidegrees.
type temp struct {
	label string
	milli int64
	ok    bool
}

// cpuTemp picks the CPU temperature of a CPU chip (see cpuChips).
func (c *chip) cpuTemp() (float64, bool) {
	temps := make(map[int]temp, len(c.temp))
	for _, n := range c.temp {
		var t temp
		if s, ok := readSmall(c.file("temp", n, "_input")); ok {
			t.milli, t.ok = parseValue(s)
		}
		if l, ok := readSmall(c.file("temp", n, "_label")); ok {
			t.label = clean(l)
		}
		temps[n] = t
	}
	byLabel := func(match func(string) bool) (temp, bool) {
		for _, n := range c.temp {
			if t := temps[n]; match(t.label) {
				return t, true
			}
		}
		return temp{}, false
	}
	var pick temp
	found := false
	switch c.name {
	case "k10temp", "zenpower":
		for _, l := range []string{"Tctl", "Tdie"} {
			if pick, found = byLabel(func(s string) bool { return s == l }); found {
				break
			}
		}
		if !found {
			pick, found = temps[1], slices.Contains(c.temp, 1)
		}
	case "coretemp":
		pick, found = byLabel(func(s string) bool { return strings.HasPrefix(s, "Package id") })
		if !found { // the hottest core
			for _, n := range c.temp {
				if t := temps[n]; strings.HasPrefix(t.label, "Core") && t.ok && (!found || t.milli > pick.milli) {
					pick, found = t, true
				}
			}
		}
	case "cpu_thermal":
		pick, found = temps[1], slices.Contains(c.temp, 1)
	}
	if !found || !pick.ok || pick.milli < minTempC*1000 || pick.milli > maxTempC*1000 {
		return 0, false
	}
	return float64(pick.milli) / 1000, true
}

// Read returns the fans of every chip that is not a GPU and the CPU
// temperature of the first CPU chip. It reads only the chips' name, fanN_input,
// fanN_label, tempN_input and tempN_label files. ErrUnavailable when the tree
// is missing or holds nothing usable.
func (r *Reader) Read() (Reading, error) {
	chips, ok := r.listChips()
	if !ok {
		return Reading{}, ErrUnavailable
	}
	var out Reading
	cpuSeen := false
	seen := map[string]int{} // chips with fans by identity name: the second is name#2
	for i := range chips {
		c := &chips[i]
		c.scan()
		if len(c.fans) > 0 && !gpuChips[c.name] {
			base := idName(c.name)
			seen[base]++
			c.idName = base
			if k := seen[base]; k > 1 {
				c.idName = base + "#" + strconv.Itoa(k)
			}
		}
		if !cpuSeen && cpuChips[c.name] {
			cpuSeen = true
			out.CPUTempC, out.HasCPUTemp = c.cpuTemp()
		}
		if !gpuChips[c.name] {
			out.Fans = c.fanReadings(out.Fans)
		}
	}
	if len(out.Fans) == 0 && !out.HasCPUTemp {
		return Reading{}, ErrUnavailable
	}
	return out, nil
}
