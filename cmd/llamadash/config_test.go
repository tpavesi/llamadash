package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llamadash.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigFileSetsOptions(t *testing.T) {
	p := writeConf(t, "# settings\n\nLLAMADASH_URL=http://localhost:8011\nLLAMADASH_KWH_PRICE = \"0.3031\"\nexport LLAMADASH_CURRENCY='gbp'\nLLAMADASH_DB=/tmp/x.db\nLLAMA_API_KEY=filekey\n")
	for _, args := range [][]string{{"--config", p}, {"--config=" + p}, {"-config", p}} {
		o, err, code := parseArgs(args, envFn(nil), &bytes.Buffer{})
		if code != -1 || err != nil {
			t.Fatalf("%v: code %d err %v", args, code, err)
		}
		if o.url != "http://localhost:8011" || o.kwhPrice != 0.3031 || o.currency != "GBP" || o.db != "/tmp/x.db" || o.apiKey != "filekey" {
			t.Fatalf("%v: %+v", args, o)
		}
	}
	// the env variable names the file too
	o, _, code := parseArgs(nil, envFn(map[string]string{"LLAMADASH_CONFIG": p}), &bytes.Buffer{})
	if code != -1 || o.kwhPrice != 0.3031 {
		t.Fatalf("LLAMADASH_CONFIG: code %d %+v", code, o)
	}
}

func TestConfigPrecedence(t *testing.T) {
	p := writeConf(t, "LLAMADASH_URL=http://localhost:8011\nLLAMADASH_KWH_PRICE=0.3031\nLLAMADASH_CURRENCY=GBP\n")
	env := envFn(map[string]string{"LLAMADASH_KWH_PRICE": "0.20"})
	o, _, code := parseArgs([]string{"--config", p, "--currency", "EUR"}, env, &bytes.Buffer{})
	if code != -1 {
		t.Fatal(code)
	}
	if o.url != "http://localhost:8011" || o.kwhPrice != 0.20 || o.currency != "EUR" {
		t.Fatalf("flag > env > file: %+v", o)
	}
}

func TestConfigUserFileOverridesSystemFile(t *testing.T) {
	home := t.TempDir()
	user := filepath.Join(home, ".config", "llamadash", "llamadash.conf")
	if err := os.MkdirAll(filepath.Dir(user), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("LLAMADASH_KWH_PRICE=0.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := readFile
	defer func() { readFile = old }()
	readFile = func(p string) ([]byte, error) {
		if p == systemConfig {
			return []byte("LLAMADASH_KWH_PRICE=0.3031\nLLAMADASH_CURRENCY=GBP\n"), nil
		}
		return os.ReadFile(p)
	}
	o, _, code := parseArgs(nil, envFn(map[string]string{"HOME": home}), &bytes.Buffer{})
	if code != -1 || o.kwhPrice != 0.25 || o.currency != "GBP" {
		t.Fatalf("code %d %+v", code, o)
	}
	// no files at all is fine
	readFile = func(p string) ([]byte, error) { return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist} }
	if o, _, code := parseArgs(nil, envFn(map[string]string{"HOME": home}), &bytes.Buffer{}); code != -1 || o.kwhPrice != 0 {
		t.Fatalf("no files: code %d %+v", code, o)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"LLAMADASH_KWH_PRISE=0.3\n":           "unknown setting",
		"just words\n":                        "expected NAME=value",
		"LLAMADASH_KWH_PRICE=abc\n":           "kwh-price",
		"LLAMA_API_KEY=SECRETKEY123\nBOGUS=1": "unknown setting",
	}
	for body, want := range cases {
		var errb bytes.Buffer
		_, _, code := parseArgs([]string{"--config", writeConf(t, body)}, envFn(nil), &errb)
		if code != 2 || !strings.Contains(errb.String(), want) {
			t.Errorf("%q: code %d stderr %q", body, code, errb.String())
		}
		if strings.Contains(errb.String(), "SECRETKEY123") {
			t.Errorf("%q: stderr shows a value: %q", body, errb.String())
		}
	}
	var errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "none.conf")
	if _, _, code := parseArgs([]string{"--config", missing}, envFn(nil), &errb); code != 2 || !strings.Contains(errb.String(), "none.conf") {
		t.Errorf("missing named file: code %d stderr %q", code, errb.String())
	}
}
