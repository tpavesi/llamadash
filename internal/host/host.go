// Package host reads machine-wide CPU load, memory and disk figures from /proc.
package host

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Info is one reading of the host.
type Info struct {
	CPUPct               float64 // all cores, 0..100
	HasCPU               bool    // false on the first read (no earlier counters to difference)
	Cores                int
	Load1, Load5, Load15 float64

	MemTotalKB, MemAvailKB, SwapTotalKB, SwapFreeKB uint64

	DiskTotal, DiskFree uint64 // bytes, the filesystem holding "/"
	HasDisk             bool
}

// ErrUnavailable means /proc/stat cannot be read (not Linux, or no procfs).
var ErrUnavailable = errors.New("host: /proc not available")

const defaultRoot = "/proc"

// Reader turns successive /proc/stat reads into a CPU percentage.
type Reader struct {
	mu                  sync.Mutex // Read may run on a goroutine that outlives a poll
	root                string
	prevTotal, prevIdle uint64
	havePrev            bool
}

// NewReader returns a Reader over root; "" means /proc.
func NewReader(procRoot string) *Reader {
	if procRoot == "" {
		procRoot = defaultRoot
	}
	return &Reader{root: procRoot}
}

// cpuTimes parses /proc/stat: the aggregate counters (idle includes iowait) and
// the number of cpuN lines. ok is false when there is no usable aggregate line.
func cpuTimes(stat string) (total, idle uint64, cores int, ok bool) {
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			if _, err := strconv.Atoi(f[0][3:]); err == nil {
				cores++
			}
			continue
		}
		if len(f) < 5 || ok { // user nice system idle at least
			continue
		}
		var v [8]uint64 // user nice system idle iowait irq softirq steal
		good := true
		for i := 0; i < len(v) && i+1 < len(f); i++ {
			n, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				good = false
				break
			}
			v[i] = n
			total += n
		}
		if !good {
			total = 0
			continue
		}
		idle, ok = v[3]+v[4], true
	}
	return total, idle, cores, ok
}

// kbFields extracts "Key:  123 kB" values for the wanted keys.
func kbFields(meminfo string, keys ...string) map[string]uint64 {
	out := make(map[string]uint64, len(keys))
	for _, line := range strings.Split(meminfo, "\n") {
		k, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		for _, want := range keys {
			if k != want {
				continue
			}
			if f := strings.Fields(rest); len(f) > 0 {
				if n, err := strconv.ParseUint(f[0], 10, 64); err == nil {
					out[k] = n
				}
			}
		}
	}
	return out
}

// finite parses a float, mapping junk, NaN and infinities to zero.
func finite(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// Read returns the current host figures. The first read has HasCPU false. A
// missing or unreadable /proc/stat is ErrUnavailable; other files that are
// missing or malformed leave their fields zero.
func (r *Reader) Read() (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	statB, err := os.ReadFile(filepath.Join(r.root, "stat"))
	if err != nil {
		return Info{}, ErrUnavailable
	}
	var in Info
	total, idle, cores, ok := cpuTimes(string(statB))
	in.Cores = cores
	if ok {
		if r.havePrev && total > r.prevTotal && idle >= r.prevIdle {
			dt, di := float64(total-r.prevTotal), float64(idle-r.prevIdle)
			in.CPUPct, in.HasCPU = min(max(100*(1-di/dt), 0), 100), true
		}
		r.prevTotal, r.prevIdle, r.havePrev = total, idle, true
	} else {
		r.havePrev = false
	}
	if b, err := os.ReadFile(filepath.Join(r.root, "meminfo")); err == nil {
		m := kbFields(string(b), "MemTotal", "MemAvailable", "SwapTotal", "SwapFree")
		in.MemTotalKB, in.MemAvailKB = m["MemTotal"], m["MemAvailable"]
		in.SwapTotalKB, in.SwapFreeKB = m["SwapTotal"], m["SwapFree"]
	}
	if b, err := os.ReadFile(filepath.Join(r.root, "loadavg")); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			in.Load1, in.Load5, in.Load15 = finite(f[0]), finite(f[1]), finite(f[2])
		}
	}
	in.DiskTotal, in.DiskFree, in.HasDisk = diskUsage("/")
	return in, nil
}
