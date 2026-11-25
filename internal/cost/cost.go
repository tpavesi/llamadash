// Package cost formats money amounts and parses the energy price.
package cost

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// symbols are the currencies with a symbol and a minor unit.
var symbols = map[string]struct{ major, minor string }{
	"GBP": {"£", "p"},
	"EUR": {"€", "c"},
	"USD": {"$", "c"},
}

// Format formats amount in currency. GBP, EUR and USD use their symbol from one unit
// up ("£1.37") and the minor unit below it ("8.6p": one decimal under 10, none from
// 10); any other code follows the amount ("1.37 CHF"); "" gives the bare amount.
// A negative or non-finite amount is "-".
func Format(amount float64, currency string) string {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return "-"
	}
	currency = strings.ToUpper(currency)
	sym, ok := symbols[currency]
	switch {
	case !ok && currency == "":
		return fmt.Sprintf("%.2f", amount)
	case !ok:
		return fmt.Sprintf("%.2f %s", amount, currency)
	}
	minor := amount * 100
	switch {
	case minor >= 99.5: // would print as 100 minor units
		return fmt.Sprintf("%s%.2f", sym.major, amount)
	case minor >= 9.95: // would print as 10.0
		return fmt.Sprintf("%.0f%s", minor, sym.minor)
	}
	return fmt.Sprintf("%.1f%s", minor, sym.minor)
}

// Minor returns the minor-unit symbol of a currency with one ("p" for GBP, "c" for
// EUR and USD), and false for any other code.
func Minor(currency string) (string, bool) {
	sym, ok := symbols[strings.ToUpper(currency)]
	return sym.minor, ok
}

// ParsePrice parses a price per kWh: a finite number above 0 and below 1000.
func ParsePrice(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("price %q is not a number", s)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v >= 1000 {
		return 0, errors.New("price must be above 0 and below 1000")
	}
	return v, nil
}
