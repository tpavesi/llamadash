// Package fakeserver is a scripted llama-server stand-in for tests.
package fakeserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// Response is one canned HTTP response.
type Response struct {
	Status int
	Body   []byte
}

// Step holds the response of every endpoint for one position in the script.
type Step struct{ Health, Props, Slots, Metrics Response }

// Server is a started fake server that counts requests per path.
type Server struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

// Hits returns how many times path (for example "/metrics") was requested.
func (s *Server) Hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func handler(steps []Step, s *Server) http.Handler {
	pick := func(path string, st Step) Response {
		switch path {
		case "/health":
			return st.Health
		case "/props":
			return st.Props
		case "/slots":
			return st.Slots
		case "/metrics":
			return st.Metrics
		}
		return Response{Status: http.StatusNotFound}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := s.hits[r.URL.Path]
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		if len(steps) == 0 || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		resp := pick(r.URL.Path, steps[min(n, len(steps)-1)])
		if resp.Status == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	})
}

// New starts a server; request N to an endpoint gets steps[min(N, len-1)].
func New(steps []Step) *httptest.Server { return NewCounting(steps).Server }

// NewCounting is New but also exposes request counts.
func NewCounting(steps []Step) *Server {
	s := &Server{hits: map[string]int{}}
	s.Server = httptest.NewServer(handler(steps, s))
	return s
}

// Switch is a started server on one fixed address whose behaviour a test changes
// between polls: Serve answers from a script, Refuse drops every connection
// without an answer (to the client, a transport error like a dead server).
type Switch struct {
	*httptest.Server
	mu     sync.Mutex
	h      http.Handler // nil: refuse
	counts *Server      // request counts since the last Serve
}

// NewSwitch starts a Switch serving steps.
func NewSwitch(steps []Step) *Switch {
	sw := &Switch{}
	sw.Serve(steps)
	sw.Server = httptest.NewServer(http.HandlerFunc(sw.handle))
	return sw
}

// Serve answers from steps, with fresh request counts.
func (sw *Switch) Serve(steps []Step) {
	counts := &Server{hits: map[string]int{}}
	h := handler(steps, counts)
	sw.mu.Lock()
	sw.h, sw.counts = h, counts
	sw.mu.Unlock()
}

// Refuse closes every connection, including kept-alive ones, without answering.
func (sw *Switch) Refuse() {
	sw.mu.Lock()
	sw.h = nil
	sw.mu.Unlock()
}

// Hits returns how many times path was answered since the last Serve.
func (sw *Switch) Hits(path string) int {
	sw.mu.Lock()
	counts := sw.counts
	sw.mu.Unlock()
	return counts.Hits(path)
}

func (sw *Switch) handle(w http.ResponseWriter, r *http.Request) {
	sw.mu.Lock()
	h := sw.h
	sw.mu.Unlock()
	if h == nil {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	h.ServeHTTP(w, r)
}

// FromTestdata builds one Step per slots_seq/NNN.json file in dir (a testdata/<build>
// directory). Health and Props come from health.json / props.json; /metrics is the 501 body.
func FromTestdata(t testing.TB, dir string) []Step {
	t.Helper()
	read := func(p ...string) []byte {
		b, err := os.ReadFile(filepath.Join(append([]string{dir}, p...)...))
		if err != nil {
			t.Fatalf("fakeserver: %v", err)
		}
		return b
	}
	health, props, m501 := read("health.json"), read("props.json"), read("metrics_501.json")
	files, err := filepath.Glob(filepath.Join(dir, "slots_seq", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("fakeserver: no slots_seq in %s", dir)
	}
	sort.Strings(files)
	var steps []Step
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			Slots json.RawMessage `json:"slots"`
		}
		if err := json.Unmarshal(b, &w); err != nil {
			t.Fatalf("fakeserver: %s: %v", f, err)
		}
		steps = append(steps, Step{
			Health:  Response{200, health},
			Props:   Response{200, props},
			Slots:   Response{200, w.Slots},
			Metrics: Response{501, m501},
		})
	}
	return steps
}
