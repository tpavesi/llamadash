package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "docker", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const llamaID = "f00df00d0123456789abcdef0123456789abcdef0123456789abcdeff00df00d"

func TestParsePSMatchesPort(t *testing.T) {
	ps := fixture(t, "ps.txt")
	if id, ok := parsePS(ps, 8011); !ok || id != llamaID {
		t.Fatalf("8011 -> %q %v", id, ok)
	}
	for _, p := range []int{18011, 801, 8081, 5432} {
		if id, ok := parsePS(ps, p); ok {
			t.Errorf("port %d matched %q", p, id)
		}
	}
	v6 := []byte("\n\ngarbage\nab\tname\n" + "abc123\tx\t0.0.0.0:9000->80/tcp, [::]:9000->80/tcp\r\n")
	if id, ok := parsePS(v6, 9000); !ok || id != "abc123" {
		t.Fatalf("ipv6 form: %q %v", id, ok)
	}
	if _, ok := parsePS([]byte("abc\tx\t[::]:9000->80/tcp"), 9000); !ok {
		t.Fatal("ipv6-only line did not match")
	}
	if _, ok := parsePS([]byte("-rf\tx\t0.0.0.0:9000->80/tcp"), 9000); ok {
		t.Fatal("non-hex id accepted")
	}
	if _, ok := parsePS(nil, 8011); ok {
		t.Fatal("empty output matched")
	}
}

func TestParseInspect(t *testing.T) {
	in, err := parseInspect(fixture(t, "inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != "llama-server-main" || in.Health != "unhealthy" || in.State != "running" || in.Restarts != 0 {
		t.Fatalf("%+v", in)
	}
	want := time.Date(2026, 11, 24, 22, 36, 47, 0, time.UTC)
	if !in.StartedAt.Truncate(time.Second).Equal(want) {
		t.Fatalf("StartedAt = %v", in.StartedAt)
	}
}

func TestParseInspectNoHealthcheck(t *testing.T) {
	in, err := parseInspect([]byte(`[{"Name":"/x","RestartCount":3,"State":{"Status":"restarting","StartedAt":"0001-01-01T00:00:00Z"}}]`))
	if err != nil || in.Health != "" || in.State != "restarting" || in.Restarts != 3 || !in.StartedAt.IsZero() {
		t.Fatalf("%+v %v", in, err)
	}
}

func TestParseInspectGarbage(t *testing.T) {
	for _, s := range []string{"", "[]", "{}", `[{"Name":"/x","State":{`, "null", `[{}]`} {
		if _, err := parseInspect([]byte(s)); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
}

func TestParseInspectCleansStrings(t *testing.T) {
	long := strings.Repeat("é", 100)
	in, err := parseInspect([]byte(`[{"Name":"/a\u001b[31mb\nc` + long + `","State":{"Status":"run\u0000ning","Health":{"Status":"` + long + `"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(in.Name+in.State, "\x1b\n\x00") || in.State != "running" {
		t.Fatalf("control characters kept: %q %q", in.Name, in.State)
	}
	if len(in.Name) > 64 || len(in.Health) > 64 || !strings.HasPrefix(in.Name, "a[31mbc") {
		t.Fatalf("not capped: %d %d %q", len(in.Name), len(in.Health), in.Name)
	}
}

func TestLookupNoDocker(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := Lookup(context.Background(), 8011); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestClassifyStderr(t *testing.T) {
	down := "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
	perm := "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock"
	if err := classify(down); !errors.Is(err, ErrDaemonDown) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("daemon down -> %v", err)
	}
	if err := classify(perm); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrDaemonDown) {
		t.Fatalf("permission denied -> %v", err)
	}
	if classify("something else") != nil {
		t.Fatal("unknown stderr classified")
	}
}

func TestFixtureScrubbed(t *testing.T) {
	b := string(fixture(t, "inspect.json"))
	for _, bad := range []string{"--model", "gguf", "PATH=", "/app/", "10.10.", "172.", "sha256:"} {
		if strings.Contains(b, bad) {
			t.Errorf("inspect.json contains %q", bad)
		}
	}
	ps := string(fixture(t, "ps.txt"))
	if strings.Contains(ps, "10.10.") { // the live host's address
		t.Error("ps.txt contains the live host's address")
	}
	if !strings.Contains(ps, llamaID+"\tllama-server-main\t192.0.2.10:8011->8081/tcp") { // an invented id
		t.Errorf("ps.txt lost the llama row:\n%s", ps)
	}
}
