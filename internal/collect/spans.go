package collect

import "time"

// spanRing runs alongside a history ring: for each sample pushed it keeps when
// the period that sample covers began, the time of the push before it (the first
// sample's own time). The oldest entry is where the ring's span starts, whatever
// the poll interval was when each sample was taken.
type spanRing struct {
	starts []time.Time
	start  int
	n      int
	last   time.Time
}

func newSpanRing(capacity int) *spanRing {
	return &spanRing{starts: make([]time.Time, max(capacity, 1))}
}

// push records a sample taken at now.
func (s *spanRing) push(now time.Time) {
	from := s.last
	if from.IsZero() {
		from = now
	}
	s.last = now
	if s.n < len(s.starts) {
		s.starts[(s.start+s.n)%len(s.starts)] = from
		s.n++
		return
	}
	s.starts[s.start] = from
	s.start = (s.start + 1) % len(s.starts)
}

// values is when each sample's period began, oldest first (nil for an empty ring).
func (s *spanRing) values() []time.Time {
	if s.n == 0 {
		return nil
	}
	out := make([]time.Time, s.n)
	for i := range out {
		out[i] = s.starts[(s.start+i)%len(s.starts)]
	}
	return out
}

// from is when the oldest sample's period began; zero for an empty ring.
func (s *spanRing) from() time.Time {
	if s.n == 0 {
		return time.Time{}
	}
	return s.starts[s.start]
}
