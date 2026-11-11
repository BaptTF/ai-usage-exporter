package cursor

import (
	"regexp"
	"strconv"
	"strings"
)

// monthlyPricePattern accepts only an explicit monthly price, such as
// "$20/mo", "$20 / month", "USD 20 per month" or "20/monthly".
//
// Anything else — a yearly price, a bare number, an unknown currency — is
// treated as unknown. The exporter never invents a subscription cost.
var monthlyPricePattern = regexp.MustCompile(`(?i)^\s*(?:usd\s*)?\$?\s*([0-9]+(?:\.[0-9]+)?)\s*(?:/|per\s+)\s*(?:mo|month|monthly)\s*$`)

// ParseMonthlyPriceUSD parses a Cursor plan price string into USD per month.
func ParseMonthlyPriceUSD(price string) (float64, bool) {
	m := monthlyPricePattern.FindStringSubmatch(strings.TrimSpace(price))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}
