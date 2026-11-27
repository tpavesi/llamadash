package ui

import "math"

// powerRole is the colour of the total draw: its share of the power supply's
// wattage as shown (a whole percentage), amber from 80%, red from 95%. Without
// a wattage there is nothing to measure against: the plain value colour.
func powerRole(powerW, psuWatts float64) role {
	if psuWatts <= 0 {
		return rValue
	}
	switch pct := math.Round(powerW / psuWatts * 100); {
	case pct >= 95:
		return rBad
	case pct >= 80:
		return rWarn
	}
	return rGood
}
