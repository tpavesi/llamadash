package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"llamadash/internal/stats"
)

// batch is a set of queued writes.
type batch struct {
	samples  []Sample
	requests []stats.Finished
	usage    map[string]Usage // local day -> deltas
}

func (b *batch) len() int { return len(b.samples) + len(b.requests) + len(b.usage) }

// take returns the queued writes and empties the queue.
func (b *batch) take() batch {
	out := *b
	*b = batch{usage: map[string]Usage{}}
	return out
}

// putBack returns a batch that failed to write to the front of the queue, keeping the
// queue bounds (older items are the ones dropped) and merging its usage.
func (b *batch) putBack(old batch) {
	b.samples = keepLast(append(old.samples, b.samples...), queueSamples)
	b.requests = keepLast(append(old.requests, b.requests...), queueRequests)
	for day, u := range old.usage {
		b.usage[day] = b.usage[day].add(u)
	}
}

func keepLast[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// run is the writer goroutine.
func (s *Store) run(tick <-chan time.Time) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			s.finalErr = s.flush(ctx)
			cancel()
			return
		case o := <-s.ops:
			o.res <- o.fn(context.Background(), s.conn)
		case <-tick:
			s.autoFlush()
		case <-s.wake:
			s.autoFlush()
		}
	}
}

// autoFlush flushes unless retries are paused after repeated failures.
func (s *Store) autoFlush() {
	s.mu.Lock()
	paused := !s.pauseUntil.IsZero() && s.cfg.now().Before(s.pauseUntil)
	s.mu.Unlock()
	if !paused {
		s.flush(context.Background())
	}
}

// flush writes the queue in one transaction. On failure the batch goes back to the queue
// and the error is kept for Err.
func (s *Store) flush(ctx context.Context) error {
	s.mu.Lock()
	b := s.q.take()
	s.mu.Unlock()
	if b.len() == 0 {
		return nil
	}
	err := writeBatch(ctx, s.conn, b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if err != nil {
		s.q.putBack(b)
		s.err = fmt.Errorf("store: write: %w", err)
		s.fails++
		if s.fails >= maxFails {
			s.pauseUntil = s.cfg.now().Add(pauseFor)
		}
		return s.err
	}
	s.err, s.fails, s.pauseUntil = nil, 0, time.Time{}
	return nil
}

func writeBatch(ctx context.Context, c *sql.Conn, b batch) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(b.samples) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO samples(at, gen_rate, prompt_rate,
			processing, queued, gpu_w, cpu_w, gpu_util, gpu_mem_pct, gpu_temp, host_cpu, host_mem_kb,
			proc_cpu, proc_rss_kb) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, x := range b.samples {
			if _, err := st.ExecContext(ctx, x.At.UnixMilli(), orNull(x.GenRate), orNull(x.PromptRate), x.Processing,
				x.Queued, orNull(x.GPUPowerW), orNull(x.CPUPowerW), orNull(x.GPUUtil), orNull(x.GPUMemPct),
				orNull(x.GPUTempC), orNull(x.HostCPUPct), uintOrNull(x.HostMemUsedKB, x.HasHostMem),
				orNull(x.ProcCPUPct), uintOrNull(x.ProcRSSKB, x.HasProcRSS)); err != nil {
				return err
			}
		}
	}
	if len(b.requests) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT INTO requests(at, slot, task, prompt_tokens,
			cache_share, generated, duration_ms, ttft_ms, gen_rate, approx) VALUES(?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, f := range b.requests {
			if _, err := st.ExecContext(ctx, f.At.UnixMilli(), f.SlotID, f.TaskID, f.PromptTokens,
				f.CacheShare, f.Generated, f.Duration.Milliseconds(), f.TTFT.Milliseconds(), f.GenRate,
				f.Approx); err != nil {
				return err
			}
		}
	}
	for day, u := range b.usage {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily(day, gen_tokens, prompt_tokens, requests,
			gpu_wh, cpu_wh, cost, priced_wh) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(day) DO UPDATE SET
			gen_tokens = gen_tokens + excluded.gen_tokens,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			requests = requests + excluded.requests,
			gpu_wh = gpu_wh + excluded.gpu_wh,
			cpu_wh = cpu_wh + excluded.cpu_wh,
			cost = cost + excluded.cost,
			priced_wh = priced_wh + excluded.priced_wh`,
			day, u.GenTokens, u.PromptTokens, u.Requests, u.GPUEnergyWh, u.CPUEnergyWh, u.Cost,
			pricedWh(u)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// clampU64 fits v into SQLite's signed 64-bit integer.
func clampU64(v uint64) int64 { return int64(min(v, math.MaxInt64)) }

// orNull binds a sample value: NULL (no reading) when it is NaN or infinite.
func orNull(v float64) any {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return v
}

// uintOrNull binds a memory figure: NULL when there is no reading.
func uintOrNull(v uint64, ok bool) any {
	if !ok {
		return nil
	}
	return clampU64(v)
}

// finite maps NaN and ±Inf to 0.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// sanitised returns u with every field that is not finite or is negative set to 0, so one
// bad delta cannot poison the day's pending sum it is merged into.
func (u Usage) sanitised() Usage {
	nn := func(v float64) float64 { return max(finite(v), 0) }
	return Usage{
		GenTokens: max(u.GenTokens, 0), PromptTokens: max(u.PromptTokens, 0), Requests: max(u.Requests, 0),
		GPUEnergyWh: nn(u.GPUEnergyWh), CPUEnergyWh: nn(u.CPUEnergyWh), Cost: nn(u.Cost),
	}
}

// pricedWh is the part of u's energy whose cost is in u.Cost: all of it when a price
// was set (a cost was computed), none otherwise.
func pricedWh(u Usage) float64 {
	if u.Cost > 0 {
		return u.GPUEnergyWh + u.CPUEnergyWh
	}
	return 0
}
