package store

import (
	"database/sql"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Energy recorded while no price was set is costed when a price is set, once.
func TestUnpricedEnergyCostedWhenPriceSet(t *testing.T) {
	s, _, _ := openTest(t, config{})
	now := base
	yesterday := now.Add(-24 * time.Hour)
	s.AddUsage(yesterday, Usage{GPUEnergyWh: 1000})           // no price that day
	s.AddUsage(now, Usage{GPUEnergyWh: 200, CPUEnergyWh: 69}) // no price this morning
	if err := s.Flush(); err != nil {                         // a run has one price, so the two never share a batch
		t.Fatal(err)
	}
	s.AddUsage(now, Usage{GPUEnergyWh: 100, Cost: 0.1 * 0.3031}) // priced when recorded
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	today, _, all, err := s.Totals(now)
	if err != nil {
		t.Fatal(err)
	}
	if !near(today.Cost, 0.1*0.3031) || !near(all.Cost, 0.1*0.3031) {
		t.Fatalf("before a price: today %v all %v", today.Cost, all.Cost)
	}
	for i := 0; i < 2; i++ { // the second call must not charge again
		if err := s.SetTariff(now, 0.3031, "GBP"); err != nil {
			t.Fatal(err)
		}
	}
	today, _, all, err = s.Totals(now)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0.369 * 0.3031; !near(today.Cost, want) {
		t.Fatalf("today %v, want %v", today.Cost, want)
	}
	if want := 1.369 * 0.3031; !near(all.Cost, want) {
		t.Fatalf("all time %v, want %v", all.Cost, want)
	}
	// later energy is priced as it is recorded, and a new price leaves past days alone
	s.AddUsage(now, Usage{GPUEnergyWh: 1000, Cost: 0.3031})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTariff(now, 0.50, "GBP"); err != nil {
		t.Fatal(err)
	}
	if _, _, all, _ = s.Totals(now); !near(all.Cost, 2.369*0.3031) {
		t.Fatalf("after a price change: %v, want %v", all.Cost, 2.369*0.3031)
	}
	// a price of 0 (cost off) charges nothing
	s.AddUsage(now, Usage{GPUEnergyWh: 500})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTariff(now, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, all, _ = s.Totals(now); !near(all.Cost, 2.369*0.3031) {
		t.Fatalf("price 0 charged: %v", all.Cost)
	}
}

// A version-1 file gains priced_wh: what its cost already covers at the stored
// price, so only the unpriced part is charged afterwards.
func TestMigrateV1PricedEnergy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{}, schemaV1...)
	stmts = append(stmts,
		// 269 Wh recorded without a price, then 1 Wh at 0.3031
		`INSERT INTO daily(day, gpu_wh, cpu_wh, cost) VALUES('2026-11-25', 200, 70, 0.0003031)`,
		`INSERT INTO daily(day, gpu_wh, cpu_wh, cost) VALUES('2026-11-24', 1000, 0, 0.3031)`,
		`INSERT INTO daily(day, gpu_wh, cpu_wh, cost) VALUES('2026-11-23', 500, 0, 0)`,
		`INSERT INTO tariff(at, price, currency) VALUES(1, 0.3031, 'GBP')`,
		`PRAGMA user_version = 1`)
	if _, err := db.Exec(strings.Join(stmts, ";")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := reopen(t, path)
	defer s.Close()
	if err := s.SetTariff(time.Now(), 0.3031, "GBP"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.rdb.Query("SELECT day, cost, priced_wh FROM daily ORDER BY day")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]float64{"2026-11-23": 0.5 * 0.3031, "2026-11-24": 0.3031, "2026-11-25": 0.27 * 0.3031}
	for rows.Next() {
		var day string
		var cost, priced float64
		if err := rows.Scan(&day, &cost, &priced); err != nil {
			t.Fatal(err)
		}
		if !near(cost, want[day]) {
			t.Errorf("%s: cost %v, want %v", day, cost, want[day])
		}
		delete(want, day)
	}
	if len(want) != 0 {
		t.Errorf("missing days: %v", want)
	}
}
