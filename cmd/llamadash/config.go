package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// systemConfig is the settings file every user reads when it exists.
const systemConfig = "/etc/llamadash.conf"

// configKeys are the settings a config file may hold: the same names as the
// environment variables.
var configKeys = map[string]bool{
	"LLAMADASH_URL":       true,
	"LLAMADASH_KWH_PRICE": true,
	"LLAMADASH_CURRENCY":  true,
	"LLAMADASH_DB":        true,
	"LLAMA_API_KEY":       true,
	"LLAMADASH_PSU_WATTS": true,
	// the learned chart tops: llamadash writes these itself (see scalesave.go)
	"LLAMADASH_AXIS_POWER_W":    true,
	"LLAMADASH_AXIS_GEN_TPS":    true,
	"LLAMADASH_AXIS_PROMPT_TPS": true,
	"LLAMADASH_KNOWN_FANS":      true, // also written by llamadash (see knownfans.go)
}

// readFile is replaced in tests.
var readFile = os.ReadFile

// configArg returns the value of --config in args ("" when absent), without
// parsing the other flags.
func configArg(args []string) (string, bool) {
	for i, a := range args {
		if a == "--" {
			break
		}
		name := strings.TrimLeft(a, "-")
		if len(name) == len(a) { // not a flag
			continue
		}
		if v, ok := strings.CutPrefix(name, "config="); ok {
			return v, true
		}
		if name == "config" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// configPaths lists the files to read, later ones overriding earlier ones, and
// whether a missing file is an error (it is when the user named the file).
func configPaths(args []string, env func(string) string) (paths []string, explicit bool) {
	if p, ok := configArg(args); ok {
		return []string{p}, true
	}
	if p := env("LLAMADASH_CONFIG"); p != "" {
		return []string{p}, true
	}
	paths = []string{systemConfig}
	dir := env("XDG_CONFIG_HOME")
	if !isAbs(dir) {
		dir = ""
		if home := env("HOME"); isAbs(home) {
			dir = filepath.Join(home, ".config")
		}
	}
	if dir != "" {
		paths = append(paths, filepath.Join(dir, "llamadash", "llamadash.conf"))
	}
	return paths, false
}

// isAbs reports whether p is absolute, also for a Unix path seen on Windows.
func isAbs(p string) bool { return path.IsAbs(filepath.ToSlash(p)) || filepath.IsAbs(p) }

// parseConfig reads KEY=VALUE lines; blank lines and lines starting with # are
// skipped, a value may be wrapped in single or double quotes. An unknown key is an
// error, so a misspelt setting is not silently ignored.
func parseConfig(data []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok {
			return nil, fmt.Errorf("line %d: expected NAME=value", n)
		}
		if !configKeys[k] {
			return nil, fmt.Errorf("line %d: unknown setting %q", n, k)
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// withConfig returns env extended by the config files: a variable set in the
// environment wins, then the user's file, then the system file. src is the file
// that was read last (the named file, else the user's file when it exists, else
// the system file when it exists; "" for none): the one learned settings go to.
// The error names the file at fault and never quotes a value.
func withConfig(args []string, env func(string) string) (_ func(string) string, src string, _ error) {
	paths, explicit := configPaths(args, env)
	vals := map[string]string{}
	for _, p := range paths {
		data, err := readFile(p)
		if err != nil {
			if !explicit && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return env, "", fmt.Errorf("%s: %v", p, err)
		}
		m, err := parseConfig(data)
		if err != nil {
			return env, "", fmt.Errorf("%s: %v", p, err)
		}
		src = p
		for k, v := range m {
			vals[k] = v
		}
	}
	return func(k string) string {
		if v := env(k); v != "" {
			return v
		}
		return vals[k]
	}, src, nil
}
