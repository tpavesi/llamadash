package stats

import (
	"reflect"
	"testing"
)

func TestRingWraps(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 5; i++ {
		r.Push(float64(i))
	}
	if got, want := r.Values(), []float64{3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRingPartialAndDegenerate(t *testing.T) {
	r := NewRing(3)
	r.Push(1)
	if got := r.Values(); !reflect.DeepEqual(got, []float64{1}) {
		t.Fatalf("got %v", got)
	}
	z := NewRing(0)
	z.Push(1) // must not panic
	if len(z.Values()) > 1 {
		t.Fatalf("zero-capacity ring grew: %v", z.Values())
	}
}
