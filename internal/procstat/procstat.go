// Package procstat reads CPU, memory and thread figures for one process, found
// by its command name, from /proc. It reads only comm, stat and status: never
// cmdline or environ.
package procstat

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info is one reading of the process.
type Info struct {
	PID     int
	CPUPct  float64 // 100 = one full core
	HasCPU  bool    // false on the first read and after a pid change
	RSSKB   uint64
	Threads int
}

var (
	// ErrNotFound means no process has the wanted comm.
	ErrNotFound = errors.New("procstat: process not found")
	// ErrUnavailable means the proc root cannot be listed.
	ErrUnavailable = errors.New("procstat: /proc not available")
)

const (
	defaultRoot = "/proc"
	clockTicks  = 100.0 // USER_HZ: fixed at 100 on Linux
)

// Reader finds the process once, caches its pid and differences its CPU ticks.
type Reader struct {
	mu   sync.Mutex // Read may run on a goroutine that outlives a poll
	root string
	comm string

	pid      int // cached; 0 = scan
	prevTick uint64
	prevAt   time.Time
	havePrev bool
}

// NewReader returns a Reader over root ("" means /proc) for processes whose
// /proc/<pid>/comm equals comm exactly.
func NewReader(procRoot, comm string) *Reader {
	if procRoot == "" {
		procRoot = defaultRoot
	}
	return &Reader{root: procRoot, comm: comm}
}

func (r *Reader) read(pid int, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.root, strconv.Itoa(pid), name))
	return string(b), err == nil
}

// commMatches reports whether the process's comm is exactly the wanted name.
func (r *Reader) commMatches(pid int) bool {
	s, ok := r.read(pid, "comm")
	return ok && strings.TrimSuffix(s, "\n") == r.comm
}

// parseStatus extracts VmRSS (kB) and Threads from /proc/<pid>/status.
func parseStatus(s string) (rssKB uint64, threads int) {
	for _, line := range strings.Split(s, "\n") {
		k, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		switch k {
		case "VmRSS":
			rssKB, _ = strconv.ParseUint(f[0], 10, 64)
		case "Threads":
			threads, _ = strconv.Atoi(f[0])
		}
	}
	return rssKB, threads
}

// parseTicks returns utime+stime from a /proc/<pid>/stat line. The line is split
// after the last ')' because comm may contain spaces and parentheses.
func parseTicks(stat string) (uint64, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:]) // f[0] is the state, field 3 of the line
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64) // utime, field 14
	s, err2 := strconv.ParseUint(f[12], 10, 64) // stime, field 15
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + s, true
}

// scan lists the numeric proc entries and returns the matching pid with the
// largest RSS, or 0.
func (r *Reader) scan() (int, error) {
	ents, err := os.ReadDir(r.root)
	if err != nil {
		return 0, ErrUnavailable
	}
	best, bestRSS := 0, uint64(0)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !r.commMatches(pid) {
			continue
		}
		s, _ := r.read(pid, "status")
		rss, _ := parseStatus(s)
		if best == 0 || rss > bestRSS {
			best, bestRSS = pid, rss
		}
	}
	if best == 0 {
		return 0, ErrNotFound
	}
	return best, nil
}

// Read returns the process's current figures. The cached pid is kept while it
// still has the wanted comm; otherwise the proc root is scanned again.
func (r *Reader) Read(now time.Time) (Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pid != 0 && !r.commMatches(r.pid) {
		r.pid = 0
	}
	if r.pid == 0 {
		pid, err := r.scan()
		if err != nil {
			r.havePrev = false
			return Info{}, err
		}
		r.pid, r.havePrev = pid, false
	}
	in := Info{PID: r.pid}
	if s, ok := r.read(r.pid, "status"); ok {
		in.RSSKB, in.Threads = parseStatus(s)
	}
	s, ok := r.read(r.pid, "stat")
	ticks, parsed := parseTicks(s)
	if !ok || !parsed {
		r.havePrev = false
		return in, nil
	}
	if dt := now.Sub(r.prevAt).Seconds(); r.havePrev && dt > 0 && ticks >= r.prevTick {
		in.CPUPct, in.HasCPU = float64(ticks-r.prevTick)/clockTicks/dt*100, true
	}
	r.prevTick, r.prevAt, r.havePrev = ticks, now, true
	return in, nil
}
