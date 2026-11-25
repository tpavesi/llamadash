// Package stats turns successive /slots samples into per-slot state, rates and history.
package stats

// Ring is a fixed-capacity buffer of the most recent samples.
type Ring struct {
	buf   []float64
	start int
	n     int
}

// NewRing returns a ring holding at most capacity values (minimum 1).
func NewRing(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{buf: make([]float64, capacity)}
}

// Push appends v, overwriting the oldest value when full.
func (r *Ring) Push(v float64) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = v
		r.n++
		return
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % len(r.buf)
}

// Values returns the samples oldest first.
func (r *Ring) Values() []float64 {
	out := make([]float64, r.n)
	for i := range out {
		out[i] = r.buf[(r.start+i)%len(r.buf)]
	}
	return out
}
