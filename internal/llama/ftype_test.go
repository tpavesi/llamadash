package llama

import "testing"

func TestShortFtype(t *testing.T) {
	cases := map[string]string{
		"Q4_K - Medium":           "Q4_K_M",
		"Q4_K - Small":            "Q4_K_S",
		"Q3_K - Large":            "Q3_K_L",
		"Q5_K - Medium":           "Q5_K_M",
		"Q6_K":                    "Q6_K",
		"Q8_0":                    "Q8_0",
		"Q4_0":                    "Q4_0",
		"F16":                     "F16",
		"BF16":                    "BF16",
		"all F32":                 "F32",
		"IQ4_XS - 4.25 bpw":       "IQ4_XS",
		"IQ2_XXS - 2.0625 bpw":    "IQ2_XXS",
		"IQ3_S mix - 3.66 bpw":    "IQ3_S",
		"Q4_K - Medium (guessed)": "Q4_K_M",
		" q8_0 ":                  "Q8_0",
		"":                        "",
		"unknown, may not work":   "",
		"mostly":                  "",
		"Q4_K\x1b[31m":            "",
		"AVERYLONGNAME_1234567":   "",
	}
	for in, want := range cases {
		if got := ShortFtype(in); got != want {
			t.Errorf("ShortFtype(%q) = %q, want %q", in, got, want)
		}
	}
}
