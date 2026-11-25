package stats

import (
	"sort"
	"time"

	"llamadash/internal/llama"
)

const (
	emaAlpha = 0.5
	minDt    = 50 * time.Millisecond
	maxDt    = 30 * time.Second // well above the 10s maximum interval plus poll time
)

// State is what a slot is doing.
type State int

const (
	Idle State = iota
	PromptEval
	Generating
)

func (s State) String() string {
	switch s {
	case PromptEval:
		return "prompt eval"
	case Generating:
		return "generating"
	}
	return "idle"
}

// SlotView is a slot sample plus derived values.
type SlotView struct {
	llama.Slot
	State         State
	GenRate       float64       // tok/s, smoothed; 0 when not generating
	PromptRate    float64       // tok/s, smoothed; 0 when not in prompt eval
	Elapsed       time.Duration // since this task id was first seen
	ElapsedApprox bool          // task was already running at first observation
	CtxUsed       int           // min(PromptTokens, NCtx)
	CacheShare    float64       // PromptCache/(PromptCache+PromptProcessed), 0..1
}

type slotState struct {
	task      int
	at        time.Time
	decoded   int
	processed int
	state     State
	gen       float64
	prompt    float64
	haveGen   bool
	havePr    bool
	firstSeen time.Time
	approx    bool

	samples  int        // busy samples seen for this task
	last     llama.Slot // last busy sample
	firstGen time.Time  // first sample with Decoded > 0
	genSeen  bool       // firstGen was observed after first sight (TTFT and rate are meaningful)
}

// Tracker keeps the previous sample of every slot.
type Tracker struct {
	slots  map[int]*slotState
	primed bool // an Update has happened since NewTracker/Reset

	out       []Finished // finished requests not yet drained
	tokGen    int        // token deltas not yet taken
	tokPrompt int
}

func NewTracker() *Tracker { return &Tracker{slots: map[int]*slotState{}} }

// Reset forgets all previous samples (called after an outage).
func (t *Tracker) Reset() {
	t.slots = map[int]*slotState{}
	t.primed = false
}

// firstState: on the real server n_decoded is 0 for idle slots and rises within a
// task; a reset of n_decoded at a new task was not observed, so Decoded>0 on first
// sight means the task is already generating.
func firstState(s llama.Slot) State {
	if s.Decoded > 0 {
		return Generating
	}
	return PromptEval
}

func ema(prev float64, have bool, raw float64) float64 {
	if !have {
		return raw
	}
	return emaAlpha*raw + (1-emaAlpha)*prev
}

func newState(now time.Time, s llama.Slot, approx bool) *slotState {
	return &slotState{task: s.TaskID, at: now, decoded: s.Decoded, processed: s.PromptProcessed,
		state: firstState(s), firstSeen: now, approx: approx}
}

// Update ingests one /slots sample taken at now.
func (t *Tracker) Update(now time.Time, slots []llama.Slot) (views []SlotView, totalGen, totalPrompt float64) {
	first := !t.primed
	t.primed = true
	seen := make(map[int]bool, len(slots))
	for _, s := range slots {
		seen[s.ID] = true
		v := SlotView{Slot: s}
		v.CtxUsed = min(max(s.PromptTokens, 0), max(s.NCtx, 0))
		if d := s.PromptCache + s.PromptProcessed; d > 0 && s.PromptCache >= 0 {
			v.CacheShare = float64(s.PromptCache) / float64(d)
		}
		if !s.IsProcessing {
			if st := t.slots[s.ID]; st != nil {
				t.finish(now, s.ID, st)
			}
			delete(t.slots, s.ID)
			views = append(views, v)
			continue
		}
		st := t.slots[s.ID]
		if st == nil || st.task != s.TaskID {
			if st != nil {
				t.finish(now, s.ID, st)
			}
			st = newState(now, s, first)
			if !st.approx { // a new task's counters start at 0: its first sample is exact
				t.tokGen += max(s.Decoded, 0)
				t.tokPrompt += max(s.PromptProcessed, 0)
			}
			t.slots[s.ID] = st
		} else {
			t.advance(st, now, s)
		}
		st.observe(now, s)
		v.State, v.ElapsedApprox = st.state, st.approx
		if e := now.Sub(st.firstSeen); e > 0 {
			v.Elapsed = e
		}
		if st.state == Generating {
			v.GenRate = st.gen
		} else {
			v.PromptRate = st.prompt
		}
		totalGen += v.GenRate
		totalPrompt += v.PromptRate
		views = append(views, v)
	}
	var gone []int
	for id := range t.slots {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Ints(gone)
	for _, id := range gone {
		t.finish(now, id, t.slots[id])
		delete(t.slots, id)
	}
	return views, totalGen, totalPrompt
}

// advance folds a new sample of the same task into st.
func (t *Tracker) advance(st *slotState, now time.Time, s llama.Slot) {
	t.countTokens(st, s)
	dt := now.Sub(st.at)
	if dt > maxDt || dt < 0 || s.Decoded < st.decoded || s.PromptProcessed < st.processed {
		st.state = firstState(s)
		st.gen, st.prompt, st.haveGen, st.havePr = 0, 0, false, false
		st.at, st.decoded, st.processed = now, s.Decoded, s.PromptProcessed
		return
	}
	if s.Decoded > st.decoded {
		st.state = Generating // sticky for the task
	}
	if dt >= minDt {
		secs := dt.Seconds()
		if st.state == Generating {
			st.gen = ema(st.gen, st.haveGen, float64(s.Decoded-st.decoded)/secs)
			st.haveGen = true
			st.prompt, st.havePr = 0, false
		} else {
			st.prompt = ema(st.prompt, st.havePr, float64(s.PromptProcessed-st.processed)/secs)
			st.havePr = true
		}
	}
	st.at, st.decoded, st.processed = now, s.Decoded, s.PromptProcessed
}
