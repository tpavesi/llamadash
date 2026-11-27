package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llamadash/internal/collect"
	"llamadash/internal/fakeserver"
	"llamadash/internal/led"
	"llamadash/internal/ui"
)

func TestLEDSettings(t *testing.T) {
	parse := func(args []string, env map[string]string) (options, int, string) {
		var errs bytes.Buffer
		if env == nil {
			env = map[string]string{}
		}
		if env["LLAMADASH_CONFIG"] == "" {
			env["LLAMADASH_CONFIG"] = writeConf(t, "")
		}
		o, _, code := parseArgs(args, envFn(env), &errs)
		return o, code, errs.String()
	}
	if o, code, _ := parse(nil, nil); code != -1 || o.led || o.ledCmd != "openrgb" || o.ledDevice != "" {
		t.Errorf("defaults: code %d, %+v", code, o)
	}
	for _, v := range []string{"on", "ON", " on "} {
		if o, code, errs := parse(nil, map[string]string{"LLAMADASH_LED": v}); code != -1 || !o.led {
			t.Errorf("LLAMADASH_LED=%q: code %d led %v %s", v, code, o.led, errs)
		}
	}
	if o, code, _ := parse(nil, map[string]string{"LLAMADASH_LED": "off"}); code != -1 || o.led {
		t.Errorf("off: code %d led %v", code, o.led)
	}
	for _, v := range []string{"maybe", "1", "true", "yes"} {
		_, code, errs := parse(nil, map[string]string{"LLAMADASH_LED": v})
		if code != 2 || !strings.Contains(errs, "llamadash: LLAMADASH_LED must be on or off") {
			t.Errorf("LLAMADASH_LED=%q: code %d, %q", v, code, errs)
		}
	}
	if o, code, _ := parse([]string{"--led=false"}, map[string]string{"LLAMADASH_LED": "on"}); code != -1 || o.led {
		t.Errorf("--led=false over on: code %d led %v", code, o.led)
	}
	if o, code, _ := parse([]string{"--led"}, map[string]string{"LLAMADASH_LED": "off"}); code != -1 || !o.led {
		t.Errorf("--led over off: code %d led %v", code, o.led)
	}
	o, code, _ := parse([]string{"--led-cmd", "/opt/x/openrgb", "--led-device", "Example Board"},
		map[string]string{"LLAMADASH_LED_CMD": "/other", "LLAMADASH_LED_DEVICE": "other"})
	if code != -1 || o.ledCmd != "/opt/x/openrgb" || o.ledDevice != "Example Board" {
		t.Errorf("flags over settings: code %d, %q %q", code, o.ledCmd, o.ledDevice)
	}
	for name, env := range map[string]map[string]string{
		"device of 65 bytes":   {"LLAMADASH_LED_DEVICE": strings.Repeat("d", 65)},
		"device with a tab":    {"LLAMADASH_LED_DEVICE": "a\tb"},
		"command with newline": {"LLAMADASH_LED_CMD": "openrgb\n--x"},
		"command with escape":  {"LLAMADASH_LED_CMD": "open\x1brgb"},
		"command of 4097":      {"LLAMADASH_LED_CMD": strings.Repeat("c", 4097)},
	} {
		if _, code, errs := parse(nil, env); code != 2 || !strings.Contains(errs, "llamadash: ") {
			t.Errorf("%s: code %d, %q", name, code, errs)
		}
	}
	if _, code, errs := parse([]string{"--led-cmd", ""}, nil); code != 2 {
		t.Errorf("empty --led-cmd: code %d %q", code, errs)
	}
	if _, code, _ := parse(nil, map[string]string{"LLAMADASH_LED_DEVICE": strings.Repeat("d", 64), "LLAMADASH_LED_CMD": strings.Repeat("c", 4096)}); code != -1 {
		t.Errorf("values at the limits: code %d", code)
	}
	conf := writeConf(t, "LLAMADASH_LED=on\nLLAMADASH_LED_CMD=/opt/openrgb/openrgb\nLLAMADASH_LED_DEVICE=Example Board\n")
	o, code, errs := parse(nil, map[string]string{"LLAMADASH_CONFIG": conf})
	if code != -1 || !o.led || o.ledCmd != "/opt/openrgb/openrgb" || o.ledDevice != "Example Board" {
		t.Errorf("settings file: code %d, %+v %s", code, o, errs)
	}
}

func TestLEDState(t *testing.T) {
	for l, want := range map[ui.Level]led.State{ui.LevelOK: led.Green, ui.LevelWarn: led.Amber, ui.LevelBad: led.Red, ui.Level(9): led.Red} {
		if got := ledState(l); got != want {
			t.Errorf("ledState(%v) = %v, want %v", l, got, want)
		}
	}
}

func TestStartLED(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 1000 }
	defer func() { geteuid = old }()
	state := t.TempDir()
	// HOME as well: a Windows temp path is not an absolute XDG_STATE_HOME
	env := envFn(map[string]string{"XDG_STATE_HOME": state, "HOME": state})
	missing := filepath.Join(t.TempDir(), "no-openrgb")
	col := func(url string, mode collect.GPUMode) *collect.Collector {
		return collect.New(collect.Config{URL: url, GPU: mode})
	}
	const local, remote = "http://127.0.0.1:8080", "http://192.0.2.1:8080"
	on := options{led: true, ledCmd: missing}
	for name, ctl := range map[string]*led.Controller{
		"off":               startLED(options{ledCmd: missing}, col(local, collect.GPUAuto), env, false),
		"one-shot":          startLED(on, col(local, collect.GPUAuto), env, true),
		"remote server":     startLED(on, col(remote, collect.GPUAuto), env, false),
		"local with no-gpu": startLED(on, col(local, collect.GPUOff), env, false),
		"no state home":     startLED(on, col(local, collect.GPUAuto), envFn(nil), false),
	} {
		if ctl != nil {
			ctl.Close()
			t.Errorf("%s: got a controller", name)
		}
	}
	if es, _ := os.ReadDir(state); len(es) != 0 {
		t.Errorf("a controller that was not started left %d entries", len(es))
	}
	for name, c := range map[string]*collect.Collector{"local": col(local, collect.GPUAuto), "remote with --gpu": col(remote, collect.GPUOn)} {
		ctl := startLED(on, c, env, false)
		if ctl == nil {
			t.Fatalf("%s: no controller", name)
		}
		for range 3 {
			ctl.Offer(led.Red)
		}
		ctl.Close() // openrgb is missing: nothing ran, nothing hangs
	}
	var dirs []string
	filepath.WalkDir(state, func(p string, d os.DirEntry, _ error) error {
		if d != nil && d.IsDir() && d.Name() == "openrgb" {
			dirs = append(dirs, p)
		}
		return nil
	})
	if len(dirs) != 1 || filepath.Base(filepath.Dir(dirs[0])) != "llamadash" {
		t.Errorf("openrgb directories = %q, want one next to the default database", dirs)
	}
}

func TestOnceWithLEDWritesNothing(t *testing.T) {
	srv := fakeserver.New(fakeserver.FromTestdata(t, "../../testdata/b12679"))
	defer srv.Close()
	state := t.TempDir()
	env := map[string]string{"XDG_STATE_HOME": state, "HOME": state, "LLAMADASH_LED": "on", "LLAMADASH_LED_CMD": filepath.Join(state, "none"),
		"LLAMADASH_CONFIG": writeConf(t, "")}
	for _, args := range [][]string{{"--once"}, {"--frame", "100x40"}} {
		code, out, errs := runArgs(append(args, "--no-db", "--interval", "250ms", "--url", srv.URL), env)
		if code != 0 {
			t.Fatalf("%v: code=%d out=%s err=%s", args, code, out, errs)
		}
		if strings.Contains(out, "led: unavailable") {
			t.Errorf("%v: a lighting note in one-shot output", args)
		}
	}
	if es, _ := os.ReadDir(state); len(es) != 0 {
		t.Errorf("one-shot runs left %d entries in the state directory", len(es))
	}
}
