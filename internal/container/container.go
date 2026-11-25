// Package container reads the host's Docker containers, and the status of the one
// that publishes the dashboard's port, by running the docker CLI.
package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Info is the status of one container.
type Info struct {
	Name      string // without the leading "/"
	Health    string // "healthy", "unhealthy", "starting", or "" when the container has no healthcheck
	State     string // "running", "restarting", "exited", ...
	StartedAt time.Time
	Restarts  int
}

var (
	// ErrUnavailable means there is no docker binary, or it may not talk to the daemon.
	ErrUnavailable = errors.New("container: docker not available")
	// ErrDaemonDown means docker is installed but the daemon does not answer; it may
	// come back, so callers keep retrying.
	ErrDaemonDown = errors.New("container: docker daemon not reachable")
	// ErrNotFound means no container publishes the port.
	ErrNotFound = errors.New("container: no container publishes the port")
)

const (
	maxOutput = 1 << 20 // stdout bytes kept per docker call
	maxString = 64      // bytes kept of a name, health or state
	waitDelay = 500 * time.Millisecond
)

// Overview is one read of the host's containers: every container, and the status
// of the one publishing the dashboard's port.
type Overview struct {
	List     []Entry // every container, running or not, in docker's order; at most maxList
	Llama    Info    // the container publishing the port, when LlamaErr is nil
	LlamaErr error   // nil, ErrNotFound, or the error of its inspect call
}

// psFormat is the only docker ps output read: no image, command, labels or mounts.
const psFormat = "{{.ID}}\t{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Ports}}"

// runner runs the docker CLI with args and returns its stdout.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// Read lists every container with one `docker ps -a` and inspects the one
// publishing hostPort. The error is that of docker ps; the lookup's own outcome is
// in LlamaErr.
func Read(ctx context.Context, hostPort int) (Overview, error) { return read(ctx, run, hostPort) }

func read(ctx context.Context, run runner, hostPort int) (Overview, error) {
	out, err := run(ctx, "ps", "-a", "--no-trunc", "--format", psFormat)
	if err != nil {
		return Overview{}, err
	}
	list, id, ok := parsePSAll(out, hostPort)
	ov := Overview{List: list}
	if !ok {
		ov.LlamaErr = ErrNotFound
		return ov, nil
	}
	if out, err = run(ctx, "inspect", id); err == nil {
		ov.Llama, err = parseInspect(out)
	}
	ov.LlamaErr = err
	return ov, nil
}

// Lookup finds the container publishing hostPort and reads its status.
func Lookup(ctx context.Context, hostPort int) (Info, error) {
	ov, err := Read(ctx, hostPort)
	if err != nil {
		return Info{}, err
	}
	return ov.Llama, ov.LlamaErr
}

// capWriter keeps the first max bytes and discards the rest, so a chatty child
// never blocks on a full pipe.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay
	stdout, stderr := &capWriter{max: maxOutput}, &capWriter{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if e := classify(stderr.buf.String()); e != nil {
			return nil, e
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
}

// classify maps docker's stderr to a sentinel, or nil. "permission denied" wins:
// its message also mentions the daemon.
func classify(stderr string) error {
	switch {
	case strings.Contains(stderr, "permission denied"):
		return ErrUnavailable
	case strings.Contains(stderr, "Cannot connect to the Docker daemon"):
		return ErrDaemonDown
	}
	return nil
}

// parsePS returns the id of the first container whose Ports column publishes hostPort.
func parsePS(out []byte, hostPort int) (id string, ok bool) {
	_, id, ok = parsePSAll(out, hostPort)
	return id, ok
}

// parsePSAll reads psFormat lines: the containers (lines with at least ID, name,
// state and status; the Ports column may be missing when empty), and the id of
// the first whose Ports column, always the last, publishes hostPort. Lines of the
// older ID/name/ports form still match the port. Malformed lines are skipped.
func parsePSAll(out []byte, hostPort int) (list []Entry, id string, ok bool) {
	want := ":" + strconv.Itoa(hostPort) + "->"
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 3 || !isHex(f[0]) {
			continue
		}
		match := !ok && strings.Contains(f[len(f)-1], want)
		if match {
			id, ok = f[0], true
		}
		if len(f) < 4 || len(list) >= maxList {
			continue
		}
		e := Entry{Name: clean(f[1]), State: clean(f[2]), Status: clean(f[3]), Llama: match}
		if e.Name == "" {
			continue
		}
		e.Health = parseHealth(f[3])
		e.ExitCode, e.HasExit = parseExit(f[3])
		list = append(list, e)
	}
	return list, id, ok
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// inspected is the part of `docker inspect` that is decoded; nothing else (command
// line, environment, labels, mounts) is kept.
type inspected struct {
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status    string `json:"Status"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

// parseInspect reads the first element of `docker inspect` output.
func parseInspect(out []byte) (Info, error) {
	var v []inspected
	if err := json.Unmarshal(out, &v); err != nil {
		return Info{}, err
	}
	if len(v) == 0 {
		return Info{}, errors.New("container: empty inspect output")
	}
	e := v[0]
	info := Info{
		Name:     clean(strings.TrimPrefix(e.Name, "/")),
		State:    clean(e.State.Status),
		Restarts: e.RestartCount,
	}
	if info.Name == "" {
		return Info{}, errors.New("container: inspect output has no name")
	}
	if e.State.Health != nil {
		info.Health = clean(e.State.Health.Status)
	}
	if t, err := time.Parse(time.RFC3339Nano, e.State.StartedAt); err == nil && t.Year() > 1 {
		info.StartedAt = t
	}
	return info, nil
}

// clean drops control characters and caps s at maxString bytes on a rune boundary.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if len(s) <= maxString {
		return s
	}
	n := maxString
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
