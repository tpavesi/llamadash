package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaVersion is the PRAGMA user_version this build writes and understands.
const schemaVersion = 2

// sampleCols are the samples columns after `at`, in Sample field order. They are the
// whitelist of Series metric names.
var sampleCols = []string{
	"gen_rate", "prompt_rate", "processing", "queued", "gpu_w", "cpu_w",
	"gpu_util", "gpu_mem_pct", "gpu_temp", "host_cpu", "host_mem_kb", "proc_cpu", "proc_rss_kb",
}

// seriesCol maps an accepted metric name to its column (the same string, never caller input).
var seriesCol = func() map[string]string {
	m := make(map[string]string, len(sampleCols))
	for _, c := range sampleCols {
		m[c] = c
	}
	return m
}()

var schemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS samples(
		at INTEGER PRIMARY KEY, -- unix ms
		gen_rate REAL, prompt_rate REAL, processing INTEGER, queued INTEGER,
		gpu_w REAL, cpu_w REAL, gpu_util REAL, gpu_mem_pct REAL, gpu_temp REAL,
		host_cpu REAL, host_mem_kb INTEGER, proc_cpu REAL, proc_rss_kb INTEGER)`,
	`CREATE TABLE IF NOT EXISTS requests(
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL, -- unix ms
		slot INTEGER, task INTEGER, prompt_tokens INTEGER, cache_share REAL, generated INTEGER,
		duration_ms INTEGER, ttft_ms INTEGER, gen_rate REAL, approx INTEGER)`,
	`CREATE INDEX IF NOT EXISTS requests_at ON requests(at)`,
	`CREATE TABLE IF NOT EXISTS daily(
		day TEXT PRIMARY KEY, -- YYYY-MM-DD, local
		gen_tokens INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
		requests INTEGER NOT NULL DEFAULT 0, gpu_wh REAL NOT NULL DEFAULT 0,
		cpu_wh REAL NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS tariff(
		at INTEGER PRIMARY KEY, -- unix ms
		price REAL, currency TEXT)`,
}

// schemaV2 adds daily.priced_wh: how much of the day's energy already has its cost
// in the cost column. Energy recorded while no price was set is costed later, when
// a price is (SetTariff). Existing rows were costed at the one price in force.
var schemaV2 = []string{
	`ALTER TABLE daily ADD COLUMN priced_wh REAL NOT NULL DEFAULT 0`,
	`UPDATE daily SET priced_wh = MIN(gpu_wh + cpu_wh,
		cost * 1000.0 / (SELECT price FROM tariff ORDER BY at DESC LIMIT 1))
		WHERE cost > 0 AND COALESCE((SELECT price FROM tariff ORDER BY at DESC LIMIT 1), 0) > 0`,
}

// checkVersion reads user_version without writing anything and rejects a newer schema.
func checkVersion(ctx context.Context, c *sql.Conn, path string) (int, error) {
	var v int
	if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read %s: %w", path, err)
	}
	if v > schemaVersion {
		return v, fmt.Errorf("store: %s has schema version %d, supported %d: %w", path, v, schemaVersion, ErrNewerSchema)
	}
	return v, nil
}

// migrate switches the file to WAL and brings the schema from version v to schemaVersion.
func migrate(ctx context.Context, c *sql.Conn, v int) error {
	var mode string
	if err := c.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
		return fmt.Errorf("store: enable WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal mode is %q, want wal", mode)
	}
	if v == schemaVersion {
		return nil
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer tx.Rollback()
	var steps []string
	if v < 1 {
		steps = append(steps, schemaV1...)
	}
	if v < 2 {
		steps = append(steps, schemaV2...)
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}
