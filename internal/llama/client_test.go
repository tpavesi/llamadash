package llama

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientStatusMapping(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{{501, ErrDisabled}, {401, ErrUnauthorized}, {403, ErrUnauthorized}, {503, ErrLoading}}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		_, err := New(srv.URL, "", time.Second).Slots(context.Background())
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("%d: got %v, want %v", tc.code, err, tc.want)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "", time.Second).Slots(context.Background())
	if err == nil || errors.Is(err, ErrDisabled) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrLoading) ||
		!errors.Is(err, ErrHTTPStatus) {
		t.Errorf("500: got %v", err)
	}
}

func TestClientBearer(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	New(srv.URL, "k", time.Second).Slots(context.Background())
	New(srv.URL, "", time.Second).Slots(context.Background())
	if len(got) != 2 || got[0] != "Bearer k" || got[1] != "" {
		t.Fatalf("headers: %q", got)
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hang until the client gives up (or 5s)
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	start := time.Now()
	_, err := New(srv.URL, "", 50*time.Millisecond).Slots(context.Background())
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestClientBaseURLTrailingSlash(t *testing.T) {
	for _, base := range []string{"", "/"} {
		var path string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.Write([]byte(`[]`))
		}))
		New(srv.URL+base, "", time.Second).Slots(context.Background())
		srv.Close()
		if path != "/slots" {
			t.Errorf("base %q: path %q", base, path)
		}
	}
}

func TestClientBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 20<<20)))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "", 5*time.Second).Metrics(context.Background())
	if err == nil {
		t.Fatal("expected error for oversize body")
	}
}

func TestClientEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		case "/props":
			w.Write([]byte(`{"total_slots":2}`))
		case "/metrics":
			w.Write([]byte("llamacpp:requests_processing 2\n"))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", time.Second)
	if h, err := c.Health(context.Background()); err != nil || h.Status != "ok" {
		t.Error(h, err)
	}
	if p, err := c.Props(context.Background()); err != nil || p.TotalSlots != 2 {
		t.Error(p, err)
	}
	if m, err := c.Metrics(context.Background()); err != nil || m["requests_processing"] != 2 {
		t.Error(m, err)
	}
}
