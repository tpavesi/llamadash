package llama

import (
	"os"
	"testing"
)

func fixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSlotsRealBusy(t *testing.T) {
	s, err := ParseSlots(fixture(t, "b12679/slots_busy.json"))
	if err != nil || len(s) != 1 {
		t.Fatalf("err=%v len=%d", err, len(s))
	}
	x := s[0]
	if x.NCtx != 262144 || !x.IsProcessing || x.TaskID != 6894 || x.PromptTokens != 146649 ||
		x.PromptProcessed != 11666 || x.PromptCache != 133408 || x.Decoded != 1573 ||
		x.Remain != -1 || !x.Stream || x.Temperature != 1.0 || !x.Speculative {
		t.Fatalf("unexpected slot %+v", x)
	}
}

func TestParseSlotsShapes(t *testing.T) {
	s, err := ParseSlots(fixture(t, "synthetic/slots_two_busy.json"))
	if err != nil || len(s) != 4 {
		t.Fatalf("err=%v len=%d", err, len(s))
	}
	if s[1].Decoded != 77 || s[1].Remain != 200 {
		t.Errorf("object next_token not parsed: %+v", s[1])
	}
	if s[3].Decoded != 0 || s[3].Remain != 0 {
		t.Errorf("missing next_token: %+v", s[3])
	}
}

func TestParseSlotsNotArray(t *testing.T) {
	if _, err := ParseSlots([]byte(`{"error":{"code":501}}`)); err == nil {
		t.Error("object body should error")
	}
	for _, in := range []string{`null`, ``, `[]`} {
		s, _ := ParseSlots([]byte(in))
		if len(s) != 0 {
			t.Errorf("%q: want empty, got %v", in, s)
		}
	}
}

func TestParseProps(t *testing.T) {
	p, err := ParseProps(fixture(t, "b12679/props.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalSlots != 1 || p.NCtx != 262144 || p.ModelAlias != "example-model-27b" ||
		p.ModelFtype != "Q4_K - Medium" || p.BuildInfo != "b12679-af5c58347" ||
		!p.EndpointSlots || !p.EndpointMetrics || p.IsSleeping {
		t.Fatalf("unexpected props %+v", p)
	}
	p, _ = ParseProps([]byte(`{"endpoint_metrics":false,"default_generation_settings":{"n_ctx":4096}}`))
	if p.EndpointMetrics || p.NCtx != 4096 {
		t.Fatalf("unexpected %+v", p)
	}
}

func TestParseMetrics(t *testing.T) {
	m, err := ParseMetrics(fixture(t, "synthetic/metrics.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if m["tokens_predicted_total"] != 21370 || m["requests_deferred"] != 3 ||
		m["kv_cache_usage_ratio"] != 0.58 {
		t.Fatalf("bad values: %v", m)
	}
	if m["spec_decode_num_accepted_tokens_per_pos_total"] != 6994 {
		t.Errorf("labelled series not keyed by bare name: %v", m)
	}
	if _, ok := m["broken_value"]; ok {
		t.Error("malformed value should be skipped")
	}
	if len(m) != 8 {
		t.Errorf("want 8 series, got %d: %v", len(m), m)
	}
}

func TestParseHealth(t *testing.T) {
	h, err := ParseHealth([]byte(`{"status":"ok"}`))
	if err != nil || h.Status != "ok" {
		t.Fatalf("%+v %v", h, err)
	}
	h, _ = ParseHealth([]byte(`{"error":{"message":"Loading model"}}`))
	if h.Status != "" {
		t.Fatalf("%+v", h)
	}
}
