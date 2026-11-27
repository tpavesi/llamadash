package led

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Runner runs openrgb with args and returns its output (capped by the implementation).
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// ErrUnavailable means the openrgb program was not found.
var ErrUnavailable = errors.New("led: openrgb not available")

const maxOutput = 64 << 10 // bytes of openrgb output kept

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	b   []byte
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Command returns a Runner for the program at path (looked up on PATH when it
// has no separator). The program is looked up on every run, so one installed
// later is found without a restart.
func Command(path string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		p, err := exec.LookPath(path)
		if err != nil {
			return nil, ErrUnavailable
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, p, args...)
		cmd.WaitDelay = time.Second // do not wait forever on pipes a killed child left open
		out := &capped{max: maxOutput}
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, fmt.Errorf("led: openrgb: %w", err)
		}
		return out.b, nil
	}
}
