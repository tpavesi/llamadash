package led

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()
	base := time.Unix(1_700_000_000, 0)
	for i := range 7 {
		p := filepath.Join(dir, fmt.Sprintf("OpenRGB_%d.log", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PruneLogs(dir, 5); err != nil {
		t.Fatal(err)
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	want := []string{"OpenRGB_2.log", "OpenRGB_3.log", "OpenRGB_4.log", "OpenRGB_5.log", "OpenRGB_6.log", "sub"}
	if !slices.Equal(names, want) {
		t.Errorf("left %q, want %q", names, want)
	}
	if err := PruneLogs(dir, 5); err != nil { // nothing more to remove
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 6 {
		t.Errorf("%d entries after a second prune, want 6", len(es))
	}
	if err := PruneLogs(dir, -1); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("%d entries with keep < 0, want only the directory", len(es))
	}
}

func TestPruneLogsMissingDir(t *testing.T) {
	if err := PruneLogs(filepath.Join(t.TempDir(), "none"), 5); err != nil {
		t.Errorf("missing directory: %v", err)
	}
}
