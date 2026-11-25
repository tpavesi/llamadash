package cost

import (
	"math"
	"testing"
)

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		amount   float64
		currency string
		want     string
	}{
		{1.374, "GBP", "£1.37"},
		{0.3031, "GBP", "30p"},
		{0.086, "GBP", "8.6p"},
		{0.42, "GBP", "42p"},
		{0.999, "GBP", "£1.00"},
		{0.0996, "GBP", "10p"},
		{0, "GBP", "0.0p"},
		{3, "EUR", "€3.00"},
		{0.086, "EUR", "8.6c"},
		{1.37, "USD", "$1.37"},
		{0.05, "USD", "5.0c"},
		{2.5, "CHF", "2.50 CHF"},
		{2.5, "", "2.50"},
		{1.374, "gbp", "£1.37"},
		{math.NaN(), "GBP", "-"},
		{math.Inf(1), "GBP", "-"},
		{-1, "GBP", "-"},
		{-1, "", "-"},
	} {
		if got := Format(c.amount, c.currency); got != c.want {
			t.Errorf("Format(%v, %q) = %q, want %q", c.amount, c.currency, got, c.want)
		}
	}
}

func TestParsePrice(t *testing.T) {
	if p, err := ParsePrice("0.3031"); err != nil || p != 0.3031 {
		t.Fatalf("0.3031: %v %v", p, err)
	}
	if p, err := ParsePrice(" 999.5 "); err != nil || p != 999.5 {
		t.Fatalf("999.5: %v %v", p, err)
	}
	for _, bad := range []string{"0", "-1", "abc", "1e9", "1000", "NaN", "Inf", "", "0x1p-2junk"} {
		if p, err := ParsePrice(bad); err == nil {
			t.Errorf("ParsePrice(%q) = %v, want an error", bad, p)
		}
	}
}
