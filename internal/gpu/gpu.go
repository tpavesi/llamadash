// Package gpu queries nvidia-smi for per-GPU utilisation, memory, temperature and power.
package gpu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is one row of nvidia-smi output.
type GPU struct {
	Index                                           int
	Name                                            string
	UtilPct, MemUsedMiB, MemTotalMiB, TempC, PowerW float64
	HasUtil, HasTemp, HasPower                      bool
	FanPct                                          float64
	HasFan                                          bool
}

// ErrUnavailable means nvidia-smi is not on PATH.
var ErrUnavailable = errors.New("gpu: nvidia-smi not available")

const (
	minColumns = 7 // without fan.speed
	maxColumns = 8
)

// num parses a numeric field; "[N/A]", "[Not Supported]", NaN, infinities and
// junk report ok=false.
func num(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// Parse parses `--format=csv,noheader,nounits` output. Rows that do not have
// seven or eight columns or a numeric index are skipped. If non-blank input
// yields no valid rows, an error is returned.
func Parse(csv []byte) ([]GPU, error) {
	var out []GPU
	nonBlank := false
	for _, line := range strings.Split(string(csv), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonBlank = true
		f := strings.Split(line, ",")
		if len(f) != minColumns && len(f) != maxColumns {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		g := GPU{Index: idx, Name: strings.TrimSpace(f[1])}
		g.UtilPct, g.HasUtil = num(f[2])
		g.MemUsedMiB, _ = num(f[3])
		g.MemTotalMiB, _ = num(f[4])
		g.TempC, g.HasTemp = num(f[5])
		g.PowerW, g.HasPower = num(f[6])
		if len(f) == maxColumns {
			g.FanPct, g.HasFan = num(f[7])
		}
		out = append(out, g)
	}
	if nonBlank && len(out) == 0 {
		return nil, errors.New("gpu: unrecognised nvidia-smi output")
	}
	return out, nil
}

// Query runs nvidia-smi and parses its output. A missing binary yields
// ErrUnavailable; a non-zero exit is an ordinary error.
func Query(ctx context.Context) ([]GPU, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path,
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,fan.speed",
		"--format=csv,noheader,nounits")
	cmd.WaitDelay = time.Second // do not wait forever on pipes a killed child left open
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("gpu: nvidia-smi: %w", err)
	}
	return Parse(stdout.Bytes())
}
