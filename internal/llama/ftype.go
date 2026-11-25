package llama

import "strings"

// ShortFtype turns the server's model_ftype into the usual short name of a
// quantisation: "Q4_K - Medium" -> "Q4_K_M", "IQ4_XS - 4.25 bpw" -> "IQ4_XS",
// "all F32" -> "F32". It returns "" when the text does not look like one.
func ShortFtype(s string) string {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "(guessed)"))
	s = strings.TrimSpace(strings.TrimPrefix(s, "all "))
	name, detail, _ := strings.Cut(s, " - ")
	name = strings.TrimSpace(name)
	if f := strings.Fields(name); len(f) > 0 {
		name = f[0] // "IQ3_S mix" -> "IQ3_S"
	}
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "small":
		name += "_S"
	case "medium":
		name += "_M"
	case "large":
		name += "_L"
	}
	name = strings.ToUpper(name)
	if len(name) < 2 || len(name) > 12 {
		return ""
	}
	digit := false
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'A' && c <= 'Z', c == '_':
		default:
			return ""
		}
	}
	if !digit { // every quantisation name carries a number
		return ""
	}
	return name
}
