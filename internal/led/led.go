// Package led shows a status on the machine's RGB lighting by running the
// openrgb command. This file only parses its output and builds its arguments.
package led

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// State is what the lighting shows.
type State int

const (
	Off State = iota
	Green
	Amber
	Red
)

// Device is one entry of `openrgb --list-detailed`.
type Device struct {
	Index int
	Name  string   // cleaned, at most maxString bytes
	Type  string   // e.g. "Motherboard"
	Modes []string // mode names without quotes or brackets, at most maxModes
}

const (
	maxDevices = 32
	maxModes   = 64
	maxString  = 64
	maxIndex   = 255 // openrgb numbers devices from 0; anything larger is not a device line

	modeOff, modeStatic = "Off", "Static"
)

// colours as confirmed on real lighting: FF7E00 reads as amber, not yellow.
var colours = map[State]string{Green: "00FF00", Amber: "FF7E00", Red: "FF0000"}

// clean drops control characters and caps s at maxString bytes on a rune boundary.
func clean(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s))
	if len(s) <= maxString {
		return s
	}
	n := maxString
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// deviceLine parses "<index>: <name>" at the start of a line.
func deviceLine(line string) (Device, bool) {
	num, name, ok := strings.Cut(line, ":")
	if !ok || num == "" || strings.TrimLeft(num, "0123456789") != "" {
		return Device{}, false
	}
	idx, err := strconv.Atoi(num)
	if name = clean(name); err != nil || idx > maxIndex || name == "" {
		return Device{}, false
	}
	return Device{Index: idx, Name: name}, true
}

// modes splits a Modes line: names are separated by spaces, a name with spaces
// is in single quotes, and the current mode is in square brackets.
func modes(s string) []string {
	var out []string
	var tok strings.Builder
	quoted := false
	flush := func() {
		if m := clean(strings.Trim(tok.String(), "[]'")); m != "" && len(out) < maxModes {
			out = append(out, m)
		}
		tok.Reset()
	}
	for _, r := range s {
		switch {
		case r == '\'':
			quoted = !quoted
		case unicode.IsSpace(r) && !quoted:
			flush()
			continue
		}
		tok.WriteRune(r)
	}
	flush()
	return out
}

// ParseDevices reads `openrgb --list-detailed` output. A device starts at an
// unindented "<index>: <name>" line; the indented Type and Modes lines after it
// are its own. Everything else is skipped, so unusable input gives no devices.
// At most maxDevices are returned.
func ParseDevices(out []byte) []Device {
	var devs []Device
	cur := -1 // the device the indented lines belong to
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			cur = -1
			if d, ok := deviceLine(line); ok {
				if len(devs) == maxDevices {
					break
				}
				devs = append(devs, d)
				cur = len(devs) - 1
			}
			continue
		}
		if cur < 0 {
			continue
		}
		key, val, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch key {
		case "Type":
			devs[cur].Type = clean(val)
		case "Modes":
			devs[cur].Modes = modes(val)
		}
	}
	return devs
}

// Pick returns the devices to drive: with name == "", those whose Type is
// "Motherboard"; otherwise those whose Name contains name. Neither match
// depends on case.
func Pick(devs []Device, name string) []Device {
	var out []Device
	name = strings.ToLower(name)
	for _, d := range devs {
		if (name == "" && strings.EqualFold(d.Type, "Motherboard")) ||
			(name != "" && strings.Contains(strings.ToLower(d.Name), name)) {
			out = append(out, d)
		}
	}
	return out
}

func (d Device) has(mode string) bool { return slices.Contains(d.Modes, mode) }

// base are the arguments of every run: never talk to a running OpenRGB server,
// and keep OpenRGB's own files in a directory the dashboard owns.
func base(configDir string) []string {
	return []string{"--noautoconnect", "--config", configDir}
}

// Args is the openrgb argument list that puts every device in st (nil without
// devices). Off is the device's Off mode, or black when it has none. Only
// device numbers and fixed words are passed on, never a name openrgb printed.
func Args(devs []Device, st State, configDir string) []string {
	if len(devs) == 0 {
		return nil
	}
	args := base(configDir)
	for _, d := range devs {
		args = append(args, "-d", strconv.Itoa(d.Index))
		colour, ok := colours[st]
		switch {
		case ok:
			args = append(args, "-m", modeStatic, "-c", colour)
		case d.has(modeOff):
			args = append(args, "-m", modeOff)
		default:
			args = append(args, "-m", modeStatic, "-c", "000000")
		}
	}
	return args
}

// ListArgs is the argument list that lists devices.
func ListArgs(configDir string) []string {
	return append(base(configDir), "--list-detailed")
}
