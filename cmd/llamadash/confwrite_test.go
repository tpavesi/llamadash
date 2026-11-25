package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRewriteConf(t *testing.T) {
	const c = scalesComment
	gen80 := []confVal{numVal("LLAMADASH_AXIS_GEN_TPS", 80)}
	all := []confVal{numVal("LLAMADASH_AXIS_POWER_W", 1200), numVal("LLAMADASH_AXIS_GEN_TPS", 87.5), numVal("LLAMADASH_AXIS_PROMPT_TPS", 0.15)}
	cases := []struct {
		name, in string
		vals     []confVal
		want     string
	}{
		{"replace in place", "# x\nLLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=50\n  LLAMADASH_DB = /x\n", gen80,
			"# x\nLLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=80\n  LLAMADASH_DB = /x\n"},
		{"append", "LLAMADASH_URL=http://h:1\n", all,
			"LLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_POWER_W=1200\nLLAMADASH_AXIS_GEN_TPS=87.5\nLLAMADASH_AXIS_PROMPT_TPS=0.15\n"},
		{"replace and append", "LLAMADASH_AXIS_GEN_TPS=1\nLLAMADASH_URL=http://h:1\n", all,
			"LLAMADASH_AXIS_GEN_TPS=87.5\nLLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_POWER_W=1200\nLLAMADASH_AXIS_PROMPT_TPS=0.15\n"},
		{"crlf", "# a\r\nLLAMADASH_AXIS_GEN_TPS=1\r\n\r\nLLAMADASH_URL=http://h:1\r\n", all,
			"# a\r\nLLAMADASH_AXIS_GEN_TPS=87.5\r\n\r\nLLAMADASH_URL=http://h:1\r\n" + c + "\r\nLLAMADASH_AXIS_POWER_W=1200\r\nLLAMADASH_AXIS_PROMPT_TPS=0.15\r\n"},
		{"no trailing newline", "LLAMADASH_URL=http://h:1", gen80,
			"LLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_GEN_TPS=80\n"},
		{"replace the last line without newline", "LLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=5", gen80,
			"LLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=80"},
		{"key twice: the last one", "LLAMADASH_AXIS_GEN_TPS=1\nLLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=2\n", gen80,
			"LLAMADASH_AXIS_GEN_TPS=1\nLLAMADASH_URL=http://h:1\nLLAMADASH_AXIS_GEN_TPS=80\n"},
		{"comment naming the key", "# LLAMADASH_AXIS_GEN_TPS=10 is an example\n  #LLAMADASH_AXIS_GEN_TPS=3\n", gen80,
			"# LLAMADASH_AXIS_GEN_TPS=10 is an example\n  #LLAMADASH_AXIS_GEN_TPS=3\n" + c + "\nLLAMADASH_AXIS_GEN_TPS=80\n"},
		{"quoted and exported", "export LLAMADASH_AXIS_GEN_TPS = \"50\"\nLLAMADASH_AXIS_PROMPT_TPS='9'\n", gen80,
			"LLAMADASH_AXIS_GEN_TPS=80\nLLAMADASH_AXIS_PROMPT_TPS='9'\n"},
		{"comment line already there", "LLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_POWER_W=900\n", all,
			"LLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_POWER_W=1200\nLLAMADASH_AXIS_GEN_TPS=87.5\nLLAMADASH_AXIS_PROMPT_TPS=0.15\n"},
		{"empty file", "", gen80, c + "\nLLAMADASH_AXIS_GEN_TPS=80\n"},
		{"two groups, each under its comment", "LLAMADASH_URL=http://h:1\n", append(gen80, fansVal([]string{"it87:fan1", "it87:fan2"})),
			"LLAMADASH_URL=http://h:1\n" + c + "\nLLAMADASH_AXIS_GEN_TPS=80\n" + fansComment + "\nLLAMADASH_KNOWN_FANS=it87:fan1,it87:fan2\n"},
		{"fans replaced in place", "LLAMADASH_KNOWN_FANS=it87:fan1\n# end\n", []confVal{fansVal([]string{"it87:fan1", "it87:fan3"})},
			"LLAMADASH_KNOWN_FANS=it87:fan1,it87:fan3\n# end\n"},
		{"nothing to change", "LLAMADASH_AXIS_GEN_TPS=80\n", gen80, "LLAMADASH_AXIS_GEN_TPS=80\n"},
		{"odd lines kept", "garbage line\r\n=\n\tLLAMADASH_URL=x # y\n", gen80,
			"garbage line\r\n=\n\tLLAMADASH_URL=x # y\n" + c + "\r\nLLAMADASH_AXIS_GEN_TPS=80\r\n"},
	}
	for _, tc := range cases {
		if got := string(rewriteConf([]byte(tc.in), tc.vals)); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
		// the result parses back to the values written
		if m, err := parseConfig(rewriteConf([]byte(tc.in), tc.vals)); err == nil {
			for _, v := range tc.vals {
				if m[v.key] != v.val {
					t.Errorf("%s: %s parses as %q", tc.name, v.key, m[v.key])
				}
			}
		}
	}
}

func TestFmtConfNum(t *testing.T) {
	for v, want := range map[float64]string{150: "150", 87.5: "87.5", 0.15: "0.15", 1000: "1000", 2.5e6: "2500000"} {
		if got := fmtConfNum(v); got != want {
			t.Errorf("%v: %q", v, got)
		}
	}
}

func TestSaveConf(t *testing.T) {
	vals := []confVal{numVal("LLAMADASH_AXIS_POWER_W", 1200)}
	t.Run("rewrites and keeps the mode", func(t *testing.T) {
		for _, mode := range []fs.FileMode{0o600, 0o644} {
			dir := t.TempDir()
			p := filepath.Join(dir, "llamadash.conf")
			if err := os.WriteFile(p, []byte("LLAMA_API_KEY=k\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil { // umask
				t.Fatal(err)
			}
			if err := saveConf(p, vals); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(p)
			if string(got) != "LLAMA_API_KEY=k\n"+scalesComment+"\nLLAMADASH_AXIS_POWER_W=1200\n" {
				t.Fatalf("%q", got)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm() != mode {
				t.Errorf("mode %v, want %v", fi.Mode().Perm(), mode)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 1 {
				t.Errorf("left behind: %v", ents)
			}
		}
	})
	t.Run("re-reads the file", func(t *testing.T) {
		p := writeConf(t, "LLAMADASH_URL=http://a:1\n")
		if err := saveConf(p, vals); err != nil {
			t.Fatal(err)
		}
		// a person edits the file meanwhile
		if err := os.WriteFile(p, []byte("LLAMADASH_URL=http://b:2\nLLAMADASH_AXIS_POWER_W=1200\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := saveConf(p, []confVal{numVal("LLAMADASH_AXIS_POWER_W", 1500)}); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); string(got) != "LLAMADASH_URL=http://b:2\nLLAMADASH_AXIS_POWER_W=1500\n" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("file vanished", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "llamadash.conf")
		if err := saveConf(p, vals); err == nil {
			t.Fatal("no error")
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("created: %v", ents)
		}
	})
	t.Run("unwritable directory", func(t *testing.T) {
		p := writeConf(t, "LLAMADASH_URL=http://a:1\n")
		old := createTemp
		defer func() { createTemp = old }()
		createTemp = func(string, string) (*os.File, error) { return nil, fs.ErrPermission }
		if err := saveConf(p, vals); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("err %v", err)
		}
		if got, _ := os.ReadFile(p); string(got) != "LLAMADASH_URL=http://a:1\n" {
			t.Fatalf("changed: %q", got)
		}
	})
	t.Run("rename fails", func(t *testing.T) {
		p := writeConf(t, "LLAMADASH_URL=http://a:1\n")
		old := renameFile
		defer func() { renameFile = old }()
		renameFile = func(string, string) error { return fs.ErrPermission }
		if err := saveConf(p, vals); err == nil {
			t.Fatal("no error")
		}
		if got, _ := os.ReadFile(p); string(got) != "LLAMADASH_URL=http://a:1\n" {
			t.Fatalf("changed: %q", got)
		}
		if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
			t.Errorf("temp file left: %v", ents)
		}
	})
}
