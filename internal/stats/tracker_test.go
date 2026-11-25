package stats

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llamadash/internal/llama"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func busy(task, decoded, processed int) llama.Slot {
	return llama.Slot{ID: 0, NCtx: 262144, TaskID: task, IsProcessing: true, Decoded: decoded, PromptProcessed: processed}
}

func one(t *testing.T, tr *Tracker, at time.Time, s llama.Slot) SlotView {
	t.Helper()
	v, _, _ := tr.Update(at, []llama.Slot{s})
	if len(v) != 1 {
		t.Fatalf("want 1 view, got %d", len(v))
	}
	return v[0]
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestGenRate(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 100, 10))
	v, totalGen, _ := tr.Update(t0.Add(time.Second), []llama.Slot{busy(1, 140, 10)})
	if v[0].State != Generating || !near(v[0].GenRate, 40) || !near(totalGen, 40) {
		t.Fatalf("got %+v totalGen %v", v[0], totalGen)
	}
}

func TestRateSmoothing(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 100, 10))
	one(t, tr, t0.Add(time.Second), busy(1, 140, 10))
	v := one(t, tr, t0.Add(2*time.Second), busy(1, 160, 10))
	if !near(v.GenRate, 30) {
		t.Fatalf("GenRate %v want 30", v.GenRate)
	}
}

func TestPromptEvalThenGenerating(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 0, 0))
	v := one(t, tr, t0.Add(time.Second), busy(1, 0, 800))
	if v.State != PromptEval || !near(v.PromptRate, 800) || v.GenRate != 0 {
		t.Fatalf("got %+v", v)
	}
	v = one(t, tr, t0.Add(2*time.Second), busy(1, 10, 800))
	if v.State != Generating || v.PromptRate != 0 || !near(v.GenRate, 10) {
		t.Fatalf("got %+v", v)
	}
}

func TestTaskChangeResets(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 400, 10))
	one(t, tr, t0.Add(time.Second), busy(1, 500, 10))
	v := one(t, tr, t0.Add(2*time.Second), busy(2, 3, 10))
	if v.GenRate != 0 || v.Elapsed != 0 || v.PromptRate != 0 {
		t.Fatalf("got %+v", v)
	}
}

func TestCounterBackwardsResets(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 400, 10))
	one(t, tr, t0.Add(time.Second), busy(1, 500, 10))
	v := one(t, tr, t0.Add(2*time.Second), busy(1, 100, 10))
	if v.GenRate != 0 {
		t.Fatalf("GenRate %v", v.GenRate)
	}
}

func TestLongGapResets(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 100, 10))
	v := one(t, tr, t0.Add(60*time.Second), busy(1, 110, 10))
	if v.GenRate != 0 {
		t.Fatalf("GenRate %v", v.GenRate)
	}
}

// At --interval 10s the real gap is the interval plus the poll time, about 10.05s.
func TestTenSecondCadence(t *testing.T) {
	tr := NewTracker()
	const dt = 10050 * time.Millisecond
	one(t, tr, t0, busy(1, 100, 10))
	one(t, tr, t0.Add(dt), busy(1, 700, 10))
	v := one(t, tr, t0.Add(2*dt), busy(1, 1300, 10))
	if want := 600 / dt.Seconds(); !near(v.GenRate, want) {
		t.Fatalf("GenRate %v want %v", v.GenRate, want)
	}
}

func TestTinyGapIgnored(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 100, 10))
	one(t, tr, t0.Add(time.Second), busy(1, 140, 10))
	v := one(t, tr, t0.Add(time.Second+time.Millisecond), busy(1, 145, 10))
	if !near(v.GenRate, 40) || math.IsInf(v.GenRate, 0) || math.IsNaN(v.GenRate) {
		t.Fatalf("GenRate %v want 40", v.GenRate)
	}
}

func TestIdleSlot(t *testing.T) {
	tr := NewTracker()
	one(t, tr, t0, busy(1, 100, 10))
	one(t, tr, t0.Add(time.Second), busy(1, 140, 10))
	v := one(t, tr, t0.Add(2*time.Second), llama.Slot{TaskID: 1, NCtx: 100})
	if v.State != Idle || v.GenRate != 0 || v.PromptRate != 0 || v.Elapsed != 0 {
		t.Fatalf("got %+v", v)
	}
}

func TestElapsedApprox(t *testing.T) {
	tr := NewTracker()
	v := one(t, tr, t0, busy(1, 5, 10))
	if !v.ElapsedApprox {
		t.Fatal("first-update busy task must be approx")
	}
	one(t, tr, t0.Add(time.Second), llama.Slot{TaskID: 1})
	one(t, tr, t0.Add(2*time.Second), busy(2, 0, 0))
	v = one(t, tr, t0.Add(5*time.Second), busy(2, 0, 100))
	if v.ElapsedApprox || v.Elapsed != 3*time.Second {
		t.Fatalf("got approx=%v elapsed=%v", v.ElapsedApprox, v.Elapsed)
	}
	tr.Reset()
	v = one(t, tr, t0.Add(6*time.Second), busy(2, 0, 100))
	if !v.ElapsedApprox {
		t.Fatal("after Reset busy task must be approx")
	}
}

func TestCtxUsedClamped(t *testing.T) {
	cases := []struct{ prompt, decoded, nctx, want int }{
		{300000, 0, 262144, 262144},
		{1000, 500, 262144, 1000},
	}
	for _, c := range cases {
		tr := NewTracker()
		s := busy(1, c.decoded, 0)
		s.PromptTokens, s.NCtx = c.prompt, c.nctx
		if v := one(t, tr, t0, s); v.CtxUsed != c.want {
			t.Errorf("prompt %d: CtxUsed %d want %d", c.prompt, v.CtxUsed, c.want)
		}
	}
}

func TestCacheShare(t *testing.T) {
	tr := NewTracker()
	s := busy(1, 0, 2040)
	s.PromptCache = 131727
	if v := one(t, tr, t0, s); math.Abs(v.CacheShare-0.9847) > 1e-3 {
		t.Fatalf("CacheShare %v", v.CacheShare)
	}
	if v := one(t, NewTracker(), t0, busy(1, 0, 0)); v.CacheShare != 0 {
		t.Fatalf("CacheShare %v want 0", v.CacheShare)
	}
}

func TestRealSequence(t *testing.T) {
	files, err := filepath.Glob("../../testdata/b12679/slots_seq/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	tr := NewTracker()
	gen := false
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			TMs   int64           `json:"t_ms"`
			Slots json.RawMessage `json:"slots"`
		}
		if err := json.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		slots, err := llama.ParseSlots(w.Slots)
		if err != nil {
			t.Fatal(err)
		}
		views, _, _ := tr.Update(t0.Add(time.Duration(w.TMs)*time.Millisecond), slots)
		for _, v := range views {
			for _, r := range []float64{v.GenRate, v.PromptRate} {
				if math.IsNaN(r) || math.IsInf(r, 0) || r < 0 {
					t.Fatalf("%s: bad rate %v", f, r)
				}
			}
			if v.State == Generating && v.GenRate > 0 {
				gen = true
			}
		}
	}
	if !gen {
		t.Fatal("no Generating sample with GenRate > 0")
	}
}

func TestSlotDisappears(t *testing.T) {
	tr := NewTracker()
	a, b := busy(1, 5, 1), busy(2, 5, 1)
	b.ID = 1
	tr.Update(t0, []llama.Slot{a, b})
	v, _, _ := tr.Update(t0.Add(time.Second), []llama.Slot{a})
	if len(v) != 1 || len(tr.slots) != 1 {
		t.Fatalf("views %d tracked %d", len(v), len(tr.slots))
	}
}

func TestStateString(t *testing.T) {
	if Idle.String() != "idle" || PromptEval.String() != "prompt eval" || Generating.String() != "generating" {
		t.Fatal("bad String")
	}
}
