package main

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"llamadash/internal/ui"
)

func TestParseKnownFans(t *testing.T) {
	ids, bad := parseKnownFans(" nct6683:fan1, nct6683:fan2,,nct6683:fan1 ,bogus, x y:fan1,nct6683#2:fan10,it87:fan")
	if want := []string{"nct6683:fan1", "nct6683:fan2", "nct6683#2:fan10"}; !slices.Equal(ids, want) || bad != 3 {
		t.Fatalf("%v (%d bad), want %v and 3 bad", ids, bad, want)
	}
	var many []string
	for i := range 70 {
		many = append(many, fmt.Sprintf("c:fan%d", i))
	}
	if ids, bad := parseKnownFans(strings.Join(many, ",")); len(ids) != 64 || ids[63] != "c:fan63" || bad != 0 {
		t.Fatalf("cap: %d (%d bad)", len(ids), bad)
	}
	if ids, bad := parseKnownFans(""); ids != nil || bad != 0 {
		t.Fatalf("empty: %v %d", ids, bad)
	}
}

// The known fans setting is lenient: malformed entries are dropped with one
// warning, never an error.
func TestKnownFansSetting(t *testing.T) {
	p := writeConf(t, "LLAMADASH_KNOWN_FANS=it87:fan1,it87:fan2\n")
	var errb bytes.Buffer
	o, _, code := parseArgs([]string{"--config", p}, envFn(nil), &errb)
	if code != -1 || !slices.Equal(o.knownFans, []string{"it87:fan1", "it87:fan2"}) || errb.Len() != 0 {
		t.Fatalf("code %d %v %q", code, o.knownFans, errb.String())
	}
	errb.Reset()
	p = writeConf(t, "LLAMADASH_KNOWN_FANS=it87:fan1,???,it87\n")
	o, _, code = parseArgs([]string{"--config", p}, envFn(nil), &errb)
	lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
	if code != -1 || !slices.Equal(o.knownFans, []string{"it87:fan1"}) || len(lines) != 1 ||
		!strings.Contains(lines[0], "LLAMADASH_KNOWN_FANS") || !strings.Contains(lines[0], "ignor") {
		t.Fatalf("code %d %v %q", code, o.knownFans, errb.String())
	}
}

// A new fan is saved once, under its own comment, with whatever tops are pending.
func TestScaleSaverWritesFans(t *testing.T) {
	s, clk, rec := newTestSaver(0)
	s.OfferFans([]string{"it87:fan1"})
	waitWrite(t, rec)
	if got := rec.last(); len(got) != 1 || got[0] != fansVal([]string{"it87:fan1"}) || got[0].comment != fansComment {
		t.Fatalf("first write %v", got)
	}
	s.OfferFans([]string{"it87:fan1", "it87:fan2"})
	s.Offer(ui.Scales{GenTPS: 40})
	clk.fire(t)
	waitWrite(t, rec)
	got := rec.last()
	if len(got) != 2 || got[0] != numVal("LLAMADASH_AXIS_GEN_TPS", 40) || got[1].val != "it87:fan1,it87:fan2" {
		t.Fatalf("second write %v", got)
	}
	s.Close()
	if rec.n() != 2 {
		t.Fatalf("%d writes", rec.n())
	}
}

// End to end: the settings file gets the line under the fans comment.
func TestKnownFansWrittenToFile(t *testing.T) {
	p := writeConf(t, "LLAMADASH_URL=http://localhost:8011\nLLAMADASH_KNOWN_FANS=it87:fan1\n")
	s := newScaleSaver(p, 0, saveConf, time.Now, after)
	s.OfferFans([]string{"it87:fan1", "it87:fan2"})
	s.Close()
	got, _ := os.ReadFile(p)
	if string(got) != "LLAMADASH_URL=http://localhost:8011\nLLAMADASH_KNOWN_FANS=it87:fan1,it87:fan2\n" {
		t.Fatalf("%q", got)
	}
}
