package led

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PruneLogs removes all but the newest keep regular files in dir: openrgb
// writes a new log file on every run. A missing dir is not an error.
// Directories and links are left alone.
func PruneLogs(dir string, keep int) error {
	es, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	for _, e := range es {
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, file{e.Name(), info.ModTime()})
		}
	}
	// newest first; the name breaks ties so the result does not depend on the order read
	slices.SortFunc(files, func(a, b file) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	var first error
	for _, f := range files[min(max(keep, 0), len(files)):] {
		if err := os.Remove(filepath.Join(dir, f.name)); err != nil && first == nil && !errors.Is(err, fs.ErrNotExist) {
			first = err
		}
	}
	return first
}
