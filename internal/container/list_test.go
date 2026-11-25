package container

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func byName(list []Entry) map[string]Entry {
	m := map[string]Entry{}
	for _, e := range list {
		m[e.Name] = e
	}
	return m
}

func TestParseListFixture(t *testing.T) {
	list, id, ok := parsePSAll(fixture(t, "ps-a.txt"), 8011)
	if !ok || id != llamaID {
		t.Fatalf("llama row: %q %v", id, ok)
	}
	if len(list) != 15 {
		t.Fatalf("%d entries, want 15 (the two malformed lines skipped): %+v", len(list), list)
	}
	m := byName(list)
	for _, bad := range []string{"broken-1", "short-line", ""} {
		if _, ok := m[bad]; ok {
			t.Errorf("malformed line kept: %q", bad)
		}
	}
	type want struct {
		state, health string
		exit          int
		hasExit       bool
		llama         bool
	}
	for name, w := range map[string]want{
		"llama-server-main": {"running", "unhealthy", 0, false, true},
		"web-1":             {"running", "healthy", 0, false, false},
		"db-1":              {"running", "", 0, false, false},
		"search-1":          {"running", "starting", 0, false, false},
		"worker-1":          {"restarting", "", 0, false, false},
		"batch-1":           {"exited", "", 0, true, false},
		"importer-1":        {"exited", "", 1, true, false},
		"tts-1":             {"exited", "", 128, true, false},
		"cache-1":           {"exited", "", 137, true, false},
		"proxy-1":           {"exited", "", 143, true, false},
		"fresh-1":           {"created", "", 0, false, false},
		"pausy-1":           {"paused", "", 0, false, false},
		"zombie-1":          {"dead", "", 0, false, false},
		"going-1":           {"removing", "", 0, false, false},
	} {
		e, ok := m[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		got := want{e.State, e.Health, e.ExitCode, e.HasExit, e.Llama}
		if got != w {
			t.Errorf("%s: %+v, want %+v", name, got, w)
		}
	}
	var odd *Entry
	for i := range list {
		if strings.HasPrefix(list[i].Name, "odd") {
			odd = &list[i]
		}
	}
	if odd == nil || strings.ContainsAny(odd.Name, "\x1b\t\r\n") || odd.Name != "odd[31m-näme.v2_x" || odd.Health != "healthy" {
		t.Fatalf("odd name: %+v", odd)
	}
}

func TestParseListTolerant(t *testing.T) {
	long := strings.Repeat("n", 300)
	in := "\r\n\n\t\t\t\t\nab12\t" + long + "\trunning\tUp 1 second\r\n" +
		"ab13\tx\texited\tExited (abc) 3 minutes ago\n" +
		"ab14\ty\texited\tExited (\n" +
		"ab15\tz\texited\tExited (99999999999999999999) now\n" +
		"ab16\t\trunning\tUp\n" +
		"ab17\tw\trunning\tUp 1 hour (healthy\n"
	list, _, ok := parsePSAll([]byte(in), 8011)
	if ok {
		t.Fatal("matched a port no row publishes")
	}
	m := byName(list)
	if len(list) != 5 {
		t.Fatalf("%d entries: %+v", len(list), list)
	}
	if e := list[0]; len(e.Name) > maxString || e.State != "running" {
		t.Errorf("long name: %d bytes %+v", len(e.Name), e)
	}
	for _, n := range []string{"x", "y", "z"} {
		if m[n].HasExit {
			t.Errorf("%s: exit code from garbage: %+v", n, m[n])
		}
	}
	if m["w"].Health != "" {
		t.Errorf("unterminated health parsed: %+v", m["w"])
	}
	for _, s := range []string{"", "\n", "garbage", "\t\t\t\t\t\t", strings.Repeat("a\t", 1000)} {
		parsePSAll([]byte(s), 1) // never panics
	}
}

func TestParseListCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "%04x\tc-%d\trunning\tUp 1 hour\t\n", i+1, i)
	}
	fmt.Fprintf(&b, "ffff\tllama\trunning\tUp 1 hour\t0.0.0.0:8011->8080/tcp\n")
	list, id, ok := parsePSAll([]byte(b.String()), 8011)
	if len(list) != maxList || !ok || id != "ffff" {
		t.Fatalf("%d entries (want %d), id %q %v", len(list), maxList, id, ok)
	}
}

func TestParseHealthAndExit(t *testing.T) {
	for status, w := range map[string]struct {
		health string
		exit   int
		has    bool
	}{
		"Up 21 minutes (healthy)":         {"healthy", 0, false},
		"Up 5 seconds (health: starting)": {"starting", 0, false},
		"Up 2 hours (unhealthy)":          {"unhealthy", 0, false},
		"Exited (128) 30 minutes ago":     {"", 128, true},
		"Exited (0) About a minute ago":   {"", 0, true},
		"Created":                         {"", 0, false},
		"Up 1 hour (Paused)":              {"", 0, false},
		"Restarting (1) 12 seconds ago":   {"", 0, false},
		"Exited (-1) 3 seconds ago":       {"", 0, false},
		"Exited (1x) 3 seconds ago":       {"", 0, false},
	} {
		h := parseHealth(status)
		code, has := parseExit(status)
		if h != w.health || code != w.exit || has != w.has {
			t.Errorf("%q: %q %d %v, want %+v", status, h, code, has, w)
		}
	}
}

func TestClassify(t *testing.T) {
	type c struct {
		state, health string
		exit          int
		hasExit       bool
	}
	cases := map[c]Level{
		{"running", "", 0, false}:             LevelOK,
		{"running", "healthy", 0, false}:      LevelOK,
		{"running", "starting", 0, false}:     LevelStarting,
		{"running", "unhealthy", 0, false}:    LevelFailed,
		{"running", "weird", 0, false}:        LevelOK,
		{"restarting", "", 0, false}:          LevelStarting,
		{"restarting", "unhealthy", 0, false}: LevelStarting,
		{"created", "", 0, false}:             LevelOff,
		{"paused", "unhealthy", 0, false}:     LevelOff,
		{"removing", "", 0, false}:            LevelOff,
		{"dead", "", 0, false}:                LevelFailed,
		{"exited", "", 0, true}:               LevelOff,
		{"exited", "", 137, true}:             LevelOff,
		{"exited", "", 143, true}:             LevelOff,
		{"exited", "", 1, true}:               LevelFailed,
		{"exited", "", 2, true}:               LevelFailed,
		{"exited", "", 128, true}:             LevelFailed,
		{"exited", "", 139, true}:             LevelFailed,
		{"exited", "", 255, true}:             LevelFailed,
		{"exited", "", 0, false}:              LevelOff,
		{"", "", 0, false}:                    LevelOff,
		{"frobnicating", "healthy", 0, false}: LevelOff,
		{"Running", "", 0, false}:             LevelOff,
	}
	for k, want := range cases {
		if got := Classify(k.state, k.health, k.exit, k.hasExit); got != want {
			t.Errorf("%+v: %v, want %v", k, got, want)
		}
		e := Entry{State: k.state, Health: k.health, ExitCode: k.exit, HasExit: k.hasExit}
		if e.Level() != want {
			t.Errorf("Entry.Level %+v", k)
		}
	}
}

// One docker ps serves both the list and the lookup; the inspect call follows for
// the llama container only.
func TestReadOneps(t *testing.T) {
	var calls [][]string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch args[0] {
		case "ps":
			return fixture(t, "ps-a.txt"), nil
		case "inspect":
			return fixture(t, "inspect.json"), nil
		}
		return nil, errors.New("unexpected")
	}
	ov, err := read(context.Background(), runner, 8011)
	if err != nil || len(ov.List) != 15 || ov.LlamaErr != nil || ov.Llama.Name != "llama-server-main" {
		t.Fatalf("%+v %v", ov, err)
	}
	if len(calls) != 2 || calls[0][0] != "ps" || calls[1][0] != "inspect" || calls[1][1] != llamaID {
		t.Fatalf("calls %v", calls)
	}
	ps := strings.Join(calls[0], " ")
	if !strings.Contains(ps, "-a") || strings.Contains(ps, "Image") || strings.Contains(ps, "Command") ||
		strings.Contains(ps, "Labels") || strings.Contains(ps, "Mounts") {
		t.Fatalf("ps args %q", ps)
	}

	// no container publishes the port: the list is still there
	calls = nil
	ov, err = read(context.Background(), runner, 9999)
	if err != nil || len(ov.List) != 15 || !errors.Is(ov.LlamaErr, ErrNotFound) || len(calls) != 1 {
		t.Fatalf("not found: %+v %v %v", ov, err, calls)
	}
	// inspect fails: the list stays, the lookup carries the error
	runner2 := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "ps" {
			return fixture(t, "ps-a.txt"), nil
		}
		return nil, ErrDaemonDown
	}
	ov, err = read(context.Background(), runner2, 8011)
	if err != nil || len(ov.List) != 15 || !errors.Is(ov.LlamaErr, ErrDaemonDown) {
		t.Fatalf("inspect down: %+v %v", ov, err)
	}
	// ps fails: everything fails, with docker's error class
	for _, e := range []error{ErrUnavailable, ErrDaemonDown} {
		ov, err = read(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, e }, 8011)
		if !errors.Is(err, e) || ov.List != nil {
			t.Fatalf("ps %v: %+v %v", e, ov, err)
		}
	}
}

func TestReadNoDocker(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := Read(context.Background(), 8011); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestListFixtureScrubbed(t *testing.T) {
	ps := string(fixture(t, "ps-a.txt"))
	for _, bad := range []string{"10.10.", "sha256:", "/app/", "gguf"} {
		if strings.Contains(ps, bad) {
			t.Errorf("ps-a.txt contains %q", bad)
		}
	}
}
