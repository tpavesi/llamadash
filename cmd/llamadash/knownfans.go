package main

import (
	"strings"

	"llamadash/internal/hwmon"
)

// maxKnownFans bounds LLAMADASH_KNOWN_FANS, as the collector bounds the fans it
// remembers.
const maxKnownFans = 64

// parseKnownFans reads LLAMADASH_KNOWN_FANS, the comma-separated identities of
// the fans llamadash has seen spinning (hwmon.ID: "chip:fanN"). llamadash
// maintains the line, so it is read leniently: malformed entries are skipped and
// counted in bad, duplicates collapse, and at most maxKnownFans are kept.
func parseKnownFans(v string) (ids []string, bad int) {
	seen := map[string]bool{}
	for _, e := range strings.Split(v, ",") {
		e = strings.TrimSpace(e)
		switch {
		case e == "" || seen[e]:
		case !hwmon.ValidID(e):
			bad++
		case len(ids) < maxKnownFans:
			seen[e] = true
			ids = append(ids, e)
		}
	}
	return ids, bad
}
