package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"llamadash/internal/stats"
)

const maxBuckets = 100_000

// SetTariff records price/currency from at, but only when they differ from the latest row.
func (s *Store) SetTariff(at time.Time, price float64, currency string) error {
	if math.IsNaN(price) || math.IsInf(price, 0) {
		return fmt.Errorf("store: tariff price %v is not finite", price)
	}
	if !validCurrency(currency) {
		return fmt.Errorf("store: tariff currency %q is not empty or 3 ASCII letters", currency)
	}
	currency = strings.ToUpper(currency)
	return s.do(func(ctx context.Context, c *sql.Conn) error {
		var p float64
		var cur string
		err := c.QueryRowContext(ctx, "SELECT price, currency FROM tariff ORDER BY at DESC LIMIT 1").Scan(&p, &cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: tariff: %w", err)
		}
		if err != nil || p != price || cur != currency {
			if _, err := c.ExecContext(ctx, "INSERT OR REPLACE INTO tariff(at, price, currency) VALUES(?,?,?)",
				at.UnixMilli(), price, currency); err != nil {
				return fmt.Errorf("store: tariff: %w", err)
			}
		}
		if price <= 0 {
			return nil
		}
		// energy recorded while no price was set gets its cost now, at this price
		if _, err := c.ExecContext(ctx, `UPDATE daily SET
			cost = cost + (gpu_wh + cpu_wh - priced_wh) / 1000.0 * ?,
			priced_wh = gpu_wh + cpu_wh
			WHERE gpu_wh + cpu_wh > priced_wh + 1e-9`, price); err != nil {
			return fmt.Errorf("store: tariff: %w", err)
		}
		return nil
	})
}

// validCurrency accepts "" or exactly three ASCII letters (an ISO 4217-style code).
func validCurrency(c string) bool {
	if c == "" {
		return true
	}
	if len(c) != 3 {
		return false
	}
	for i := range len(c) {
		if b := c[i] | 0x20; b < 'a' || b > 'z' {
			return false
		}
	}
	return true
}

// Prune deletes samples older than sampleDays and requests older than requestDays
// (a value <= 0 keeps that table). daily and tariff are never touched.
func (s *Store) Prune(now time.Time, sampleDays, requestDays int) error {
	return s.do(func(ctx context.Context, c *sql.Conn) error {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: prune: %w", err)
		}
		defer tx.Rollback()
		for _, p := range []struct {
			q    string
			days int
		}{
			{"DELETE FROM samples WHERE at < ?", sampleDays},
			{"DELETE FROM requests WHERE at < ?", requestDays},
		} {
			if p.days <= 0 {
				continue
			}
			cut := now.Add(-time.Duration(p.days) * 24 * time.Hour).UnixMilli()
			if _, err := tx.ExecContext(ctx, p.q, cut); err != nil {
				return fmt.Errorf("store: prune: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: prune: %w", err)
		}
		return nil
	})
}

// RecentRequests returns up to limit stored requests, newest first.
func (s *Store) RecentRequests(limit int) ([]stats.Finished, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.rdb.Query(`SELECT at, slot, task, prompt_tokens, cache_share, generated,
		duration_ms, ttft_ms, gen_rate, approx FROM requests ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: requests: %w", err)
	}
	defer rows.Close()
	var out []stats.Finished
	for rows.Next() {
		var f stats.Finished
		var at, dur, ttft int64
		var slot, task, prompt, gen sql.NullInt64
		var share, rate sql.NullFloat64
		var approx sql.NullBool
		if err := rows.Scan(&at, &slot, &task, &prompt, &share, &gen, &dur, &ttft, &rate, &approx); err != nil {
			return nil, fmt.Errorf("store: requests: %w", err)
		}
		f.At = time.UnixMilli(at)
		f.SlotID, f.TaskID = int(slot.Int64), int(task.Int64)
		f.PromptTokens, f.Generated = int(prompt.Int64), int(gen.Int64)
		f.CacheShare, f.GenRate = share.Float64, rate.Float64
		f.Duration, f.TTFT = time.Duration(dur)*time.Millisecond, time.Duration(ttft)*time.Millisecond
		f.Approx = approx.Bool
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: requests: %w", err)
	}
	return out, nil
}

// Totals sums the daily rows of now's local date, now's local month, and all time.
func (s *Store) Totals(now time.Time) (today, month, all Totals, err error) {
	day := dayOf(now)
	if today, err = s.sumDaily("WHERE day = ?", day); err != nil {
		return
	}
	if month, err = s.sumDaily("WHERE substr(day, 1, 7) = ?", day[:7]); err != nil {
		return
	}
	all, err = s.sumDaily("")
	return
}

// sumDaily sums daily rows matching where (a constant clause, never caller input).
func (s *Store) sumDaily(where string, args ...any) (Totals, error) {
	var t Totals
	var n int64
	err := s.rdb.QueryRow(`SELECT COUNT(*), COALESCE(SUM(gen_tokens), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(requests), 0), COALESCE(SUM(gpu_wh), 0), COALESCE(SUM(cpu_wh), 0),
		COALESCE(SUM(cost), 0) FROM daily `+where, args...).
		Scan(&n, &t.GenTokens, &t.PromptTokens, &t.Requests, &t.GPUEnergyWh, &t.CPUEnergyWh, &t.Cost)
	if err != nil {
		return Totals{}, fmt.Errorf("store: totals: %w", err)
	}
	t.HasData = n > 0
	return t, nil
}

// Series splits [from, to] into buckets equal intervals and returns the mean of metric
// (a samples column name) per bucket, oldest first, each at its bucket's start. Buckets
// without data are omitted.
func (s *Store) Series(metric string, from, to time.Time, buckets int) ([]Point, error) {
	col, ok := seriesCol[metric]
	if !ok {
		return nil, fmt.Errorf("store: unknown metric %q", metric)
	}
	lo, hi := from.UnixMilli(), to.UnixMilli()
	if hi <= lo {
		return nil, fmt.Errorf("store: empty series range %v..%v", from, to)
	}
	if buckets < 1 || buckets > maxBuckets {
		return nil, fmt.Errorf("store: bucket count %d out of range 1..%d", buckets, maxBuckets)
	}
	span, n := hi-lo, int64(buckets)
	if span > math.MaxInt64/n {
		return nil, fmt.Errorf("store: series range too long")
	}
	rows, err := s.rdb.Query(`SELECT MIN((at - ?) * ? / ?, ?) AS b, AVG(`+col+`) FROM samples
		WHERE at >= ? AND at <= ? AND `+col+` IS NOT NULL GROUP BY b ORDER BY b`,
		lo, n, span, n-1, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("store: series: %w", err)
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var b int64
		var v float64
		if err := rows.Scan(&b, &v); err != nil {
			return nil, fmt.Errorf("store: series: %w", err)
		}
		out = append(out, Point{At: time.UnixMilli(lo + b*span/n), V: v})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: series: %w", err)
	}
	return out, nil
}
