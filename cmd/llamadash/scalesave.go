package main

import (
	"slices"
	"sync"
	"time"

	"llamadash/internal/ui"
)

const (
	scaleSaveEvery = 30 * time.Second // at most one write of the learned tops per this
	scaleRetry     = 10 * time.Minute // after a failed write
)

// scaleSink returns the saver of the learned chart tops for the live dashboard, or
// nil when there is nothing to write to (no settings file was read) or the run is
// one-shot (--once, --frame), which never writes.
func scaleSink(o options) *scaleSaver {
	if o.confFile == "" || o.once || o.frameW > 0 {
		return nil
	}
	return newScaleSaver(o.confFile, o.psuWatts, saveConf, time.Now, after)
}

// scaleSaver writes the learned chart tops into the settings file, off the UI
// goroutine: the first raise at once, later ones at most every scaleSaveEvery
// (several raises in that time become one write of the latest tops), and what is
// still pending on Close. After a failed write nothing is written for scaleRetry;
// the dashboard goes on with the tops for this session only. It never prints.
type scaleSaver struct {
	path  string
	psu   float64
	save  func(path string, vals []confVal) error
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	mu          sync.Mutex
	pending     *ui.Scales // tops to write; nil: none
	pendingFans []string   // known fans to write; nil: none
	lastWrite   time.Time  // last write attempt that worked
	failedAt    time.Time  // last write attempt that failed; zero after one that worked
	closed      bool

	kick      chan struct{} // a raise is pending
	done      chan struct{} // closed by Close
	stopped   chan struct{} // closed when the loop has returned
	closeOnce sync.Once
}

func newScaleSaver(path string, psu float64, save func(string, []confVal) error, now func() time.Time,
	after func(time.Duration) <-chan time.Time) *scaleSaver {
	s := &scaleSaver{path: path, psu: psu, save: save, now: now, after: after,
		kick: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

// Offer notes the current tops after a raise. It never blocks.
func (s *scaleSaver) Offer(sc ui.Scales) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pending = &sc
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default: // a kick is already waiting: it will write these
	}
}

// OfferFans notes the known fans after one was added. It never blocks.
func (s *scaleSaver) OfferFans(ids []string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.pendingFans = slices.Clone(ids)
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// nextAt is the earliest time the next write may happen. Call with mu held.
func (s *scaleSaver) nextAt() time.Time {
	var t time.Time
	if !s.lastWrite.IsZero() {
		t = s.lastWrite.Add(scaleSaveEvery)
	}
	if !s.failedAt.IsZero() {
		t = s.failedAt.Add(scaleRetry)
	}
	return t
}

func (s *scaleSaver) loop() {
	defer close(s.stopped)
	for {
		select {
		case <-s.done:
			return
		case <-s.kick:
		}
		s.mu.Lock()
		wait := s.nextAt().Sub(s.now())
		s.mu.Unlock()
		if wait > 0 {
			select {
			case <-s.done:
				return
			case <-s.after(wait):
			}
		}
		s.flush()
	}
}

// flush writes the pending tops and known fans, if any.
func (s *scaleSaver) flush() {
	s.mu.Lock()
	p, fans := s.pending, s.pendingFans
	s.pending, s.pendingFans = nil, nil
	s.mu.Unlock()
	if p == nil && fans == nil {
		return
	}
	var vals []confVal
	if p != nil {
		vals = s.vals(*p)
	}
	if fans != nil {
		vals = append(vals, fansVal(fans))
	}
	err := s.save(s.path, vals)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failedAt = s.now()
		// still to be written, unless newer values came meanwhile
		if s.pending == nil {
			s.pending = p
		}
		if s.pendingFans == nil {
			s.pendingFans = fans
		}
		return
	}
	s.lastWrite, s.failedAt = s.now(), time.Time{}
}

// vals are the settings lines for sc: every top there is, except a power top that
// is only the power supply's wattage (nothing was learned beyond it) and a value
// the settings reader would not take back (validAxis).
func (s *scaleSaver) vals(sc ui.Scales) []confVal {
	var out []confVal
	for _, v := range []struct {
		key string
		v   float64
	}{{"LLAMADASH_AXIS_POWER_W", sc.PowerW}, {"LLAMADASH_AXIS_GEN_TPS", sc.GenTPS}, {"LLAMADASH_AXIS_PROMPT_TPS", sc.PromptTPS}} {
		if validAxis(v.v) && (v.key != "LLAMADASH_AXIS_POWER_W" || v.v > s.psu) {
			out = append(out, numVal(v.key, v.v))
		}
	}
	return out
}

// Close stops the saver and writes what is pending, unless a write failed less
// than scaleRetry ago. It waits for a write in progress; later Offers are ignored.
func (s *scaleSaver) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
		<-s.stopped
		s.mu.Lock()
		blocked := !s.failedAt.IsZero() && s.now().Sub(s.failedAt) < scaleRetry
		s.mu.Unlock()
		if !blocked {
			s.flush()
		}
	})
}
