package led

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a test: Command runs this test binary as a stand-in
// for openrgb.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv("LED_HELPER") {
	case "big":
		fmt.Print(strings.Repeat("0123456789abcdef", 100<<10/16))
	case "echo":
		fmt.Print(strings.Join(os.Args[len(os.Args)-2:], " "))
	case "hang":
		time.Sleep(time.Minute)
	case "fail":
		os.Exit(3)
	default:
		return
	}
	os.Exit(0)
}

func helper(t *testing.T, mode string) Runner {
	t.Setenv("LED_HELPER", mode)
	return Command(os.Args[0])
}

func TestCommandMissing(t *testing.T) {
	_, err := Command(filepath.Join(t.TempDir(), "no-such-program"))(context.Background(), "--list-detailed")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if _, err := Command("")(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("empty path: err = %v, want ErrUnavailable", err)
	}
}

func TestCommandPassesArgs(t *testing.T) {
	out, err := helper(t, "echo")(context.Background(), "-test.run=TestHelperProcess", "--", "-m", "Off")
	if err != nil || string(out) != "-m Off" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestCommandCapsOutput(t *testing.T) {
	out, err := helper(t, "big")(context.Background(), "-test.run=TestHelperProcess")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != maxOutput {
		t.Errorf("%d bytes of output, want %d", len(out), maxOutput)
	}
}

func TestCommandFails(t *testing.T) {
	if _, err := helper(t, "fail")(context.Background(), "-test.run=TestHelperProcess"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want an exit error", err)
	}
}

func TestCommandKilledAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := helper(t, "hang")(ctx, "-test.run=TestHelperProcess")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("returned after %v", d)
	}
}
