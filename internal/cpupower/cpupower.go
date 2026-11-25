// Package cpupower reads CPU package power from the kernel's RAPL powercap
// counters (/sys/class/powercap/intel-rapl:N; AMD uses the same names).
package cpupower

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable means no readable RAPL package zone exists (not Linux, no
// powercap, or permission denied).
var ErrUnavailable = errors.New("cpupower: no readable RAPL package zone")

const (
	defaultRoot = "/sys/class/powercap"
	zonePrefix  = "intel-rapl:"
	minGap      = 50 * time.Millisecond
	maxGap      = 30 * time.Second // above the 10s maximum interval plus poll time; RAPL wraps after minutes
)

// counter is one zone's energy counter and wrap range, in microjoules.
// max is 0 when max_energy_range_uj is missing or unreadable.
type counter struct{ energy, max uint64 }

// Reader turns successive RAPL counter reads into average package power.
type Reader struct {
	mu     sync.Mutex // Read may be called from a goroutine that outlives a poll
	root   string
	prev   map[string]counter
	prevAt time.Time
}

// NewReader returns a Reader over root; "" means /sys/class/powercap.
func NewReader(root string) *Reader {
	if root == "" {
		root = defaultRoot
	}
	return &Reader{root: root}
}

// isPackageZone reports whether a powercap directory is a top-level CPU
// package zone: "intel-rapl:<N>" (no sub-zone suffix) whose name starts with
// "package-". psys, intel-rapl-mmio:* and core/uncore/dram sub-zones are
// rejected so no energy is counted twice.
func isPackageZone(dirName, nameFileContent string) bool {
	n, ok := strings.CutPrefix(dirName, zonePrefix)
	if !ok || n == "" {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.HasPrefix(strings.TrimSpace(nameFileContent), "package-")
}

// zoneDelta is the energy used between two counter values. A decrease means
// the counter wrapped: max is added once. With no usable max, or a result
// that still makes no sense, the decrease is a reset and ok is false.
func zoneDelta(prev, cur, max uint64) (uj uint64, ok bool) {
	if cur >= prev {
		return cur - prev, true
	}
	if max == 0 || prev > max {
		return 0, false
	}
	return cur + (max - prev), true
}

type gap int

const (
	gapOK      gap = iota // usable interval
	gapKeep               // too short: keep the baseline, wait
	gapReplace            // too long (or clock went backwards): replace the baseline
)

func classifyGap(dt time.Duration) gap {
	switch {
	case dt < 0, dt > maxGap:
		return gapReplace
	case dt < minGap:
		return gapKeep
	}
	return gapOK
}

// sumDelta sums the energy used by every zone present in both samples, in
// joules. ok is false when no zone is comparable or any zone was reset.
func sumDelta(prev, cur map[string]counter) (joules float64, ok bool) {
	var total uint64
	n := 0
	for name, c := range cur {
		p, seen := prev[name]
		if !seen {
			continue
		}
		d, good := zoneDelta(p.energy, c.energy, c.max)
		if !good {
			return 0, false
		}
		total += d
		n++
	}
	if n == 0 {
		return 0, false
	}
	return float64(total) / 1e6, true
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// sample reads every package zone. A zone whose counter cannot be parsed is
// skipped. ErrUnavailable means there is no zone, or none could even be opened.
func (r *Reader) sample() (map[string]counter, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, ErrUnavailable
	}
	out := map[string]counter{}
	zones, unreadable := 0, 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, zonePrefix) {
			continue
		}
		dir := filepath.Join(r.root, name)
		nb, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil || !isPackageZone(name, string(nb)) {
			continue
		}
		zones++
		raw, err := os.ReadFile(filepath.Join(dir, "energy_uj"))
		if err != nil {
			unreadable++
			continue
		}
		energy, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue // garbage: skip this zone
		}
		max, _ := readUint(filepath.Join(dir, "max_energy_range_uj"))
		out[name] = counter{energy: energy, max: max}
	}
	if zones == 0 || (len(out) == 0 && unreadable == zones) {
		return nil, ErrUnavailable
	}
	return out, nil
}

// Read returns the average CPU package power since the previous call and the
// energy used in that interval. ok is false on the first call and after a
// reset, a too-short interval (baseline kept) or a too-long one (baseline
// replaced).
func (r *Reader) Read(now time.Time) (watts, joules float64, ok bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, err := r.sample()
	if err != nil {
		r.prev = nil
		return 0, 0, false, err
	}
	if r.prev == nil {
		r.prev, r.prevAt = cur, now
		return 0, 0, false, nil
	}
	dt := now.Sub(r.prevAt)
	switch classifyGap(dt) {
	case gapKeep:
		return 0, 0, false, nil
	case gapReplace:
		r.prev, r.prevAt = cur, now
		return 0, 0, false, nil
	}
	j, good := sumDelta(r.prev, cur)
	r.prev, r.prevAt = cur, now
	if !good {
		return 0, 0, false, nil
	}
	return j / dt.Seconds(), j, true, nil
}
