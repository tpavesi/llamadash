package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"llamadash/internal/collect"
	"llamadash/internal/led"
	"llamadash/internal/store"
	"llamadash/internal/ui"
)

const (
	maxLEDCmd    = 4096 // bytes of LLAMADASH_LED_CMD
	maxLEDDevice = 64   // bytes of LLAMADASH_LED_DEVICE
)

// ledDefault reads LLAMADASH_LED: on, off or empty. ok is false for anything else.
func ledDefault(env func(string) string) (on, ok bool) {
	switch strings.ToLower(strings.TrimSpace(env("LLAMADASH_LED"))) {
	case "on":
		return true, true
	case "off", "":
		return false, true
	}
	return false, false
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ledArgs checks the lighting settings. A person sets them, so a bad one is an
// error. It returns -1 when they are fine, else the exit code.
func ledArgs(o *options, env func(string) string, stderr io.Writer) (int, error) {
	if _, ok := ledDefault(env); !ok {
		fmt.Fprintln(stderr, "llamadash: LLAMADASH_LED must be on or off")
		return 2, errors.New("bad led setting")
	}
	o.ledCmd, o.ledDevice = strings.TrimSpace(o.ledCmd), strings.TrimSpace(o.ledDevice)
	if o.ledCmd == "" || len(o.ledCmd) > maxLEDCmd || hasControl(o.ledCmd) {
		fmt.Fprintf(stderr, "llamadash: --led-cmd (LLAMADASH_LED_CMD) must be the openrgb program's name or path: at most %d bytes, no control characters\n", maxLEDCmd)
		return 2, errors.New("bad led command")
	}
	if len(o.ledDevice) > maxLEDDevice || hasControl(o.ledDevice) {
		fmt.Fprintf(stderr, "llamadash: --led-device (LLAMADASH_LED_DEVICE) must be at most %d bytes with no control characters\n", maxLEDDevice)
		return 2, errors.New("bad led device")
	}
	return -1, nil
}

// ledState is the colour for a status level.
func ledState(l ui.Level) led.State {
	switch l {
	case ui.LevelOK:
		return led.Green
	case ui.LevelWarn:
		return led.Amber
	}
	return led.Red
}

// startLED returns the lighting controller, or nil when the feature is off, the
// run is one-shot, or local sources are off (the lights belong to this machine,
// like its sensors). openrgb keeps its files next to the default database,
// whatever --db says.
func startLED(o options, c *collect.Collector, env func(string) string, oneShot bool) *led.Controller {
	if !o.led || oneShot || c == nil || !c.LocalSources() {
		return nil
	}
	db, err := defaultDBPath(geteuid(), env)
	if err != nil {
		return nil
	}
	dir := filepath.Join(filepath.Dir(db), "openrgb")
	return led.New(led.Config{
		Run:       led.Command(o.ledCmd),
		Device:    o.ledDevice,
		ConfigDir: dir,
		Lock:      ledLock(dir),
	})
}

// ledLockDir is where the lock of the lights lives: one place for every user of
// the machine, so a dashboard started by someone else is not a second owner.
// Tests replace it.
var ledLockDir = "/run/lock"

// lockFile is replaced in tests.
var lockFile = func(p string) (io.Closer, error) {
	l, err := store.Lock(p)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// ledLock returns the function that takes the lock of the lights. It fails
// when another copy holds it, which includes a lock file of another user that
// this one may not open. On a system without ledLockDir the lock is next to
// openrgb's files in dir, and then only covers copies run by the same user.
func ledLock(dir string) func() (io.Closer, error) {
	return func() (io.Closer, error) {
		p := filepath.Join(ledLockDir, "llamadash-led")
		if st, err := os.Stat(ledLockDir); err != nil || !st.IsDir() {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nopCloser{}, nil // not another copy: let the run fail and say so
			}
			p = filepath.Join(dir, "led")
		}
		l, err := lockFile(p)
		switch {
		case err == nil:
			return l, nil
		case errors.Is(err, store.ErrLocked), errors.Is(err, fs.ErrPermission):
			return nil, err
		}
		return nopCloser{}, nil
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
