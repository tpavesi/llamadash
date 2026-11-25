package procstat

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "proc", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// addProc writes <root>/<pid>/{comm,stat,status}. stat and status may be "" to omit.
func addProc(t *testing.T, root string, pid int, comm, stat, status string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"comm": comm + "\n", "stat": stat, "status": status}
	for name, content := range files {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func status(rssKB, threads int) string {
	return "Name:\tllama-server\nVmRSS:\t" + strconv.Itoa(rssKB) + " kB\nThreads:\t" + strconv.Itoa(threads) + "\n"
}

// statLine builds a /proc/<pid>/stat line with the given utime and stime ticks.
func statLine(pid int, comm string, utime, stime int) string {
	f := strings.Fields("S 1 1 1 0 -1 0 0 0 0 0 " + strconv.Itoa(utime) + " " + strconv.Itoa(stime) + " 0 0 20 0 1 0 0 0 0")
	return strconv.Itoa(pid) + " (" + comm + ") " + strings.Join(f, " ") + "\n"
}

func TestFindByComm(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, 10, "bash", statLine(10, "bash", 1, 1), status(5000, 1))
	addProc(t, root, 20, "llama-server", statLine(20, "llama-server", 1, 1), status(100, 2))
	addProc(t, root, 30, "llama-server", statLine(30, "llama-server", 1, 1), status(900, 7))
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3 4\n"), 0o644); err != nil { // not a pid
		t.Fatal(err)
	}
	in, err := NewReader(root, "llama-server").Read(t0)
	if err != nil {
		t.Fatal(err)
	}
	if in.PID != 30 || in.RSSKB != 900 || in.Threads != 7 || in.HasCPU {
		t.Errorf("got %+v, want pid 30 rss 900 threads 7 no CPU", in)
	}
}

// pid_stat_a -> pid_stat_b, 2s apart: utime+stime = 344994+19753 = 364747 and
// 345202+19755 = 364957 ticks, so 210 ticks in 2 s = 2.1 s of CPU / 2 s = 105 %.
func TestProcCPUPct(t *testing.T) {
	root := t.TempDir()
	st := fixture(t, "pid_status.txt")
	addProc(t, root, 115429, "llama-server", fixture(t, "pid_stat_a.txt"), st)
	r := NewReader(root, "llama-server")
	in, err := r.Read(t0)
	if err != nil {
		t.Fatal(err)
	}
	if in.HasCPU || in.PID != 115429 || in.RSSKB != 12263340 || in.Threads != 103 {
		t.Fatalf("first read: %+v", in)
	}
	addProc(t, root, 115429, "llama-server", fixture(t, "pid_stat_b.txt"), st)
	in, err = r.Read(t0.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !in.HasCPU || math.Abs(in.CPUPct-105) > 1e-9 {
		t.Errorf("CPUPct = %v (HasCPU %v), want 105", in.CPUPct, in.HasCPU)
	}
}

func TestCommWithParens(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, 7, "llama-server-x", statLine(7, "llama-server-x", 1, 1), status(1, 1))
	addProc(t, root, 8, "xllama-server", statLine(8, "xllama-server", 1, 1), status(1, 1))
	if _, err := NewReader(root, "llama-server").Read(t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("near-miss comm matched: %v", err)
	}
	// the stat line of a process whose comm has spaces and parentheses
	root = t.TempDir()
	addProc(t, root, 7, "llama-server", "7 (a (b) c) S 1 1 1 0 -1 0 0 0 0 0 30 12 0 0 20 0 1 0 0 0 0\n", status(1, 1))
	r := NewReader(root, "llama-server")
	if _, err := r.Read(t0); err != nil {
		t.Fatal(err)
	}
	addProc(t, root, 7, "llama-server", "7 (a (b) c) S 1 1 1 0 -1 0 0 0 0 0 130 12 0 0 20 0 1 0 0 0 0\n", status(1, 1))
	in, err := r.Read(t0.Add(time.Second)) // 100 ticks in 1 s = 100 %
	if err != nil || !in.HasCPU || math.Abs(in.CPUPct-100) > 1e-9 {
		t.Errorf("parenthesised comm: %+v, %v", in, err)
	}
}

func TestPidChangeResets(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, 30, "llama-server", statLine(30, "llama-server", 500, 500), status(900, 3))
	r := NewReader(root, "llama-server")
	if in, err := r.Read(t0); err != nil || in.PID != 30 {
		t.Fatalf("%+v, %v", in, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "30")); err != nil {
		t.Fatal(err)
	}
	addProc(t, root, 40, "llama-server", statLine(40, "llama-server", 10, 10), status(800, 3)) // far fewer ticks than pid 30
	in, err := r.Read(t0.Add(2 * time.Second))
	if err != nil || in.PID != 40 || in.HasCPU || in.CPUPct != 0 {
		t.Fatalf("after pid change: %+v, %v", in, err)
	}
	addProc(t, root, 40, "llama-server", statLine(40, "llama-server", 110, 10), status(800, 3))
	in, err = r.Read(t0.Add(3 * time.Second)) // 100 ticks in 1 s
	if err != nil || !in.HasCPU || math.Abs(in.CPUPct-100) > 1e-9 {
		t.Errorf("next read: %+v, %v", in, err)
	}
}

func TestProcNotFound(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, 10, "bash", statLine(10, "bash", 1, 1), status(5, 1))
	// cmdline and environ must never be opened: as directories, opening them would fail
	for _, name := range []string{"cmdline", "environ"} {
		if err := os.Mkdir(filepath.Join(root, "10", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(root, "llama-server")
	_, err := r.Read(t0)
	if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "cmdline") || strings.Contains(err.Error(), "environ") {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	addProc(t, root, 20, "llama-server", statLine(20, "llama-server", 1, 1), status(5, 1))
	if err := os.Mkdir(filepath.Join(root, "20", "cmdline"), 0o755); err != nil {
		t.Fatal(err)
	}
	if in, err := r.Read(t0.Add(time.Second)); err != nil || in.PID != 20 {
		t.Fatalf("found process: %+v, %v", in, err)
	}
}

func TestProcUnavailableRoot(t *testing.T) {
	_, err := NewReader(filepath.Join(t.TempDir(), "nope"), "llama-server").Read(t0)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestProcGarbage(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, 5, "llama-server", "garbage no parens", "VmRSS: lots kB\nThreads: x\n")
	addProc(t, root, 6, "llama-server", "6 (llama-server) S 1", "")
	r := NewReader(root, "llama-server")
	for i := 0; i < 2; i++ {
		in, err := r.Read(t0.Add(time.Duration(i) * time.Second))
		if err != nil || in.HasCPU || in.CPUPct != 0 {
			t.Errorf("read %d: %+v, %v", i, in, err)
		}
	}
}
