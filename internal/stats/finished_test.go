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

func sec(n float64) time.Time { return t0.Add(time.Duration(n * float64(time.Second))) }

// bs is a busy slot with an explicit prompt split.
func bs(task, decoded, cache, processed, prompt int) llama.Slot {
	return llama.Slot{ID: 0, NCtx: 4096, TaskID: task, IsProcessing: true, Decoded: decoded,
		PromptCache: cache, PromptProcessed: processed, PromptTokens: prompt}
}

// idleSlot mimics the real server: counters zero, id_task and n_prompt_tokens kept.
func idleSlot(task, prompt int) llama.Slot {
	return llama.Slot{ID: 0, NCtx: 4096, TaskID: task, PromptTokens: prompt}
}

func upd(tr *Tracker, at time.Time, s ...llama.Slot) { tr.Update(at, s) }

func only(t *testing.T, tr *Tracker) Finished {
	t.Helper()
	got := tr.Drain()
	if len(got) != 1 {
		t.Fatalf("want 1 finished, got %d: %+v", len(got), got)
	}
	return got[0]
}

func TestFinishedOnIdle(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(-1), idleSlot(0, 0)) // primed while idle: the request is seen from its start
	upd(tr, sec(0), bs(7, 0, 0, 1000, 1000))
	upd(tr, sec(1), bs(7, 0, 0, 1000, 1000))
	upd(tr, sec(2), bs(7, 10, 0, 1000, 1010))
	upd(tr, sec(12), bs(7, 410, 0, 1000, 1410))
	if got := tr.Drain(); len(got) != 0 {
		t.Fatalf("finished early: %+v", got)
	}
	upd(tr, sec(13), idleSlot(7, 1410))
	f := only(t, tr)
	if f.PromptTokens != 1000 || f.Generated != 410 || f.TTFT != 2*time.Second || f.Duration != 13*time.Second ||
		f.Approx || f.TaskID != 7 || f.SlotID != 0 || !f.At.Equal(sec(13)) || f.CacheShare != 0 {
		t.Fatalf("got %+v", f)
	}
	if math.Abs(f.GenRate-410.0/11) > 1e-9 {
		t.Fatalf("GenRate %v", f.GenRate)
	}
	// the idle slot keeps its task id: it must not be recorded again
	upd(tr, sec(14), idleSlot(7, 1410))
	if got := tr.Drain(); len(got) != 0 {
		t.Fatalf("recorded twice: %+v", got)
	}
}

func TestFinishedPromptFallbackAndCacheShare(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), bs(1, 0, 300, 100, 400))
	upd(tr, sec(1), bs(1, 20, 300, 100, 420))
	upd(tr, sec(2), idleSlot(1, 420))
	f := only(t, tr)
	if f.PromptTokens != 400 || math.Abs(f.CacheShare-0.75) > 1e-9 {
		t.Fatalf("got %+v", f)
	}
	// cache and processed both 0: PromptTokens - Decoded
	tr = NewTracker()
	upd(tr, sec(0), bs(2, 0, 0, 0, 900))
	upd(tr, sec(1), bs(2, 50, 0, 0, 950))
	upd(tr, sec(2), idleSlot(2, 950))
	if f := only(t, tr); f.PromptTokens != 900 {
		t.Fatalf("fallback: %+v", f)
	}
	tr = NewTracker()
	upd(tr, sec(0), bs(2, 0, 0, 0, 10))
	upd(tr, sec(1), bs(2, 50, 0, 0, 20))
	upd(tr, sec(2), idleSlot(2, 20))
	if f := only(t, tr); f.PromptTokens != 0 {
		t.Fatalf("floor: %+v", f)
	}
}

func TestFinishedOnTaskChange(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(-1), idleSlot(0, 0))
	upd(tr, sec(0), bs(1, 0, 0, 100, 100))
	upd(tr, sec(1), bs(1, 5, 0, 100, 105))
	upd(tr, sec(2), bs(2, 0, 0, 200, 200))
	f := only(t, tr)
	if f.TaskID != 1 || f.Generated != 5 || !f.At.Equal(sec(2)) {
		t.Fatalf("got %+v", f)
	}
	upd(tr, sec(3), bs(2, 8, 0, 200, 208))
	upd(tr, sec(5), idleSlot(2, 208))
	f = only(t, tr)
	if f.TaskID != 2 || f.Duration != 3*time.Second || f.TTFT != time.Second || f.Approx {
		t.Fatalf("task 2 not tracked fresh: %+v", f)
	}
}

func TestFinishedApprox(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), bs(1, 500, 0, 100, 600))
	upd(tr, sec(1), bs(1, 560, 0, 100, 660))
	upd(tr, sec(2), idleSlot(1, 660))
	f := only(t, tr)
	if !f.Approx || f.Generated != 560 {
		t.Fatalf("got %+v", f)
	}
	if f.GenRate != 0 || f.TTFT != 0 {
		t.Fatalf("rate/TTFT must be unknown when generating at first sight: %+v", f)
	}
}

func TestSingleSampleNoiseIgnored(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), idleSlot(0, 0))
	upd(tr, sec(1), bs(3, 0, 0, 10, 10))
	upd(tr, sec(2), idleSlot(3, 10))
	if got := tr.Drain(); len(got) != 0 {
		t.Fatalf("noise recorded: %+v", got)
	}
}

func TestShortRequestRecorded(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), idleSlot(0, 0))
	upd(tr, sec(1), bs(3, 37, 0, 10, 47))
	upd(tr, sec(2), idleSlot(3, 47))
	f := only(t, tr)
	if f.Generated != 37 || f.GenRate != 0 {
		t.Fatalf("got %+v", f)
	}
}

func TestResetDropsInFlight(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), bs(1, 5, 0, 10, 15))
	upd(tr, sec(1), bs(1, 9, 0, 10, 19))
	tr.Reset()
	upd(tr, sec(2), idleSlot(1, 19))
	if got := tr.Drain(); len(got) != 0 {
		t.Fatalf("recorded after reset: %+v", got)
	}
}

func TestSlotDisappearsFinishes(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), bs(1, 0, 0, 10, 10))
	upd(tr, sec(1), bs(1, 9, 0, 10, 19))
	upd(tr, sec(2)) // no slots at all
	f := only(t, tr)
	if f.TaskID != 1 || f.Generated != 9 {
		t.Fatalf("got %+v", f)
	}
	upd(tr, sec(3))
	if got := tr.Drain(); len(got) != 0 {
		t.Fatalf("recorded twice: %+v", got)
	}
}

func TestClockBackwards(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(10), bs(1, 0, 0, 10, 10))
	upd(tr, sec(11), bs(1, 5, 0, 10, 15))
	upd(tr, sec(5), bs(1, 9, 0, 10, 19)) // clock jumped back
	upd(tr, sec(4), idleSlot(1, 19))
	f := only(t, tr)
	if f.Duration < 0 || f.TTFT < 0 || math.IsNaN(f.GenRate) || math.IsInf(f.GenRate, 0) || f.GenRate < 0 {
		t.Fatalf("got %+v", f)
	}
}

func TestDrainEmptiesAndOrders(t *testing.T) {
	tr := NewTracker()
	for i := 1; i <= 2; i++ {
		upd(tr, sec(float64(i*10)), bs(i, 5, 0, 10, 15))
		upd(tr, sec(float64(i*10+1)), idleSlot(i, 15))
	}
	got := tr.Drain()
	if len(got) != 2 || got[0].TaskID != 1 || got[1].TaskID != 2 {
		t.Fatalf("got %+v", got)
	}
	if again := tr.Drain(); len(again) != 0 {
		t.Fatalf("second drain: %+v", again)
	}
	for i := 1; i <= 300; i++ {
		upd(tr, sec(float64(100+i*2)), bs(i, 5, 0, 10, 15))
		upd(tr, sec(float64(101+i*2)), idleSlot(i, 15))
	}
	got = tr.Drain()
	if len(got) != 256 || got[0].TaskID != 45 || got[255].TaskID != 300 {
		t.Fatalf("backlog: len %d first %d last %d", len(got), got[0].TaskID, got[len(got)-1].TaskID)
	}
}

func TestTokenDeltas(t *testing.T) {
	tr := NewTracker()
	upd(tr, sec(0), bs(1, 0, 0, 0, 0))
	upd(tr, sec(1), bs(1, 40, 0, 800, 840))
	upd(tr, sec(2), bs(1, 100, 0, 800, 900))
	if g, p := tr.TakeTokenDeltas(); g != 100 || p != 800 {
		t.Fatalf("got (%d, %d)", g, p)
	}
	if g, p := tr.TakeTokenDeltas(); g != 0 || p != 0 {
		t.Fatalf("second call (%d, %d)", g, p)
	}
	upd(tr, sec(3), bs(1, 30, 0, 100, 130)) // counters decreased
	if g, p := tr.TakeTokenDeltas(); g != 0 || p != 0 {
		t.Fatalf("decrease counted (%d, %d)", g, p)
	}
}

func TestTokenDeltasFirstSample(t *testing.T) {
	// single-sample request, not approx
	tr := NewTracker()
	upd(tr, sec(0), idleSlot(0, 0))
	upd(tr, sec(1), bs(1, 37, 0, 200, 237))
	if g, p := tr.TakeTokenDeltas(); g != 37 || p != 200 {
		t.Fatalf("single sample: (%d, %d)", g, p)
	}
	upd(tr, sec(2), idleSlot(1, 237))
	if g, p := tr.TakeTokenDeltas(); g != 0 || p != 0 {
		t.Fatalf("idle: (%d, %d)", g, p)
	}
	// prompt first seen already at 1000 processed
	upd(tr, sec(3), bs(2, 0, 0, 1000, 1000))
	if g, p := tr.TakeTokenDeltas(); g != 0 || p != 1000 {
		t.Fatalf("prompt: (%d, %d)", g, p)
	}
	// approx first sample (first Update after NewTracker/Reset) counts nothing
	tr = NewTracker()
	upd(tr, sec(0), bs(1, 500, 0, 100, 600))
	if g, p := tr.TakeTokenDeltas(); g != 0 || p != 0 {
		t.Fatalf("approx: (%d, %d)", g, p)
	}
	// task change on a busy slot: the new task's first sample counts
	upd(tr, sec(1), bs(1, 520, 0, 100, 620))
	tr.TakeTokenDeltas()
	upd(tr, sec(2), bs(2, 20, 0, 50, 70))
	if g, p := tr.TakeTokenDeltas(); g != 20 || p != 50 {
		t.Fatalf("task change: (%d, %d)", g, p)
	}
}

func TestRealSequenceFinished(t *testing.T) {
	files, err := filepath.Glob("../../testdata/b12679/slots_seq/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	tr := NewTracker()
	var all []Finished
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
		tr.Update(t0.Add(time.Duration(w.TMs)*time.Millisecond), slots)
		all = append(all, tr.Drain()...)
	}
	// the capture ends while busy; the recorded idle sample then ends the request
	idleRaw, err := os.ReadFile("../../testdata/b12679/slots_idle.json")
	if err != nil {
		t.Fatal(err)
	}
	idle, err := llama.ParseSlots(idleRaw)
	if err != nil {
		t.Fatal(err)
	}
	tr.Update(t0.Add(time.Hour), idle)
	all = append(all, tr.Drain()...)
	if len(all) != 1 {
		t.Fatalf("want exactly one finished record, got %d: %+v", len(all), all)
	}
	for _, f := range all {
		if f.PromptTokens < 0 || f.Generated < 0 || f.Duration < 0 || f.TTFT < 0 ||
			f.GenRate < 0 || math.IsNaN(f.GenRate) || math.IsInf(f.GenRate, 0) ||
			f.CacheShare < 0 || f.CacheShare > 1 {
			t.Fatalf("bad record %+v", f)
		}
	}
	if len(all) == 1 && (all[0].Generated < 5000 || all[0].PromptTokens != 11666+133408) {
		t.Fatalf("record not built from the last busy sample: %+v", all[0])
	}
}
