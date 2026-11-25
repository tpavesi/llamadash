package stats

import (
	"time"

	"llamadash/internal/llama"
)

// maxBacklog caps finished requests held between Drain calls.
const maxBacklog = 256

// Finished is one request the dashboard saw end.
type Finished struct {
	At           time.Time // when the dashboard saw it end
	SlotID       int
	TaskID       int
	PromptTokens int           // prompt size: cache + processed of the last busy sample (fallback: PromptTokens - Decoded, floor 0)
	CacheShare   float64       // 0..1, from the last busy sample
	Generated    int           // last Decoded
	Duration     time.Duration // first sight -> end
	TTFT         time.Duration // first sight -> first sample with Decoded > 0 (observed); 0 if unknown
	GenRate      float64       // Generated / (Duration - TTFT); 0 when unknown or the span < 50ms
	Approx       bool          // already running at first sight: Duration/TTFT are lower bounds
}

// Drain returns the requests finished since the last call, oldest first.
func (t *Tracker) Drain() []Finished {
	out := t.out
	t.out = nil
	return out
}

// Tokens produced after the last busy sample of a task are lost (an idle slot reads 0).
// TakeTokenDeltas returns the tokens seen since the last call: the sum of positive
// changes of Decoded and PromptProcessed between samples of the same task.
func (t *Tracker) TakeTokenDeltas() (gen, prompt int) {
	gen, prompt = t.tokGen, t.tokPrompt
	t.tokGen, t.tokPrompt = 0, 0
	return gen, prompt
}

// countTokens adds the positive counter changes from st to s.
func (t *Tracker) countTokens(st *slotState, s llama.Slot) {
	if d := s.Decoded - st.decoded; d > 0 {
		t.tokGen += d
	}
	if d := s.PromptProcessed - st.processed; d > 0 {
		t.tokPrompt += d
	}
}

// observe records the busy sample s in st.
func (st *slotState) observe(now time.Time, s llama.Slot) {
	st.samples++
	st.last = s
	if s.Decoded > 0 && st.firstGen.IsZero() {
		st.firstGen = now
		st.genSeen = st.samples > 1
	}
}

// finish turns the bookkeeping of a task that ended at now into a Finished record.
func (t *Tracker) finish(now time.Time, id int, st *slotState) {
	last := st.last
	if st.samples < 2 && last.Decoded == 0 {
		return // one sample with nothing decoded: indistinguishable from noise
	}
	f := Finished{At: now, SlotID: id, TaskID: st.task, Generated: max(last.Decoded, 0), Approx: st.approx}
	f.PromptTokens = max(last.PromptCache, 0) + max(last.PromptProcessed, 0)
	if f.PromptTokens == 0 {
		f.PromptTokens = max(last.PromptTokens-last.Decoded, 0)
	}
	if d := last.PromptCache + last.PromptProcessed; d > 0 && last.PromptCache >= 0 {
		f.CacheShare = float64(last.PromptCache) / float64(d)
	}
	f.Duration = max(now.Sub(st.firstSeen), 0)
	if st.genSeen {
		f.TTFT = min(max(st.firstGen.Sub(st.firstSeen), 0), f.Duration)
		if span := f.Duration - f.TTFT; span >= minDt {
			f.GenRate = float64(f.Generated) / span.Seconds()
		}
	}
	if len(t.out) >= maxBacklog {
		t.out = append(t.out[:0], t.out[1:]...)
	}
	t.out = append(t.out, f)
}
