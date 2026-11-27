package collect

import "testing"

func TestLocalSources(t *testing.T) {
	for _, c := range []struct {
		url  string
		mode GPUMode
		want bool
	}{
		{"http://127.0.0.1:8080", GPUAuto, true},
		{"http://localhost:8080", GPUAuto, true},
		{"http://192.0.2.1:8080", GPUAuto, false},
		{"http://192.0.2.1:8080", GPUOn, true},
		{"http://127.0.0.1:8080", GPUOff, false},
	} {
		if got := New(Config{URL: c.url, GPU: c.mode}).LocalSources(); got != c.want {
			t.Errorf("%s mode %v: LocalSources = %v, want %v", c.url, c.mode, got, c.want)
		}
	}
}
