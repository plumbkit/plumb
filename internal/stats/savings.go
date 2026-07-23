package stats

import (
	"strconv"
	"strings"
)

// FormatSavings renders a token count as a short human string ("1.2k", "850").
// The fractional form truncates (9990 → "9.9k"), not rounds, so callers get a
// stable label as a count climbs.
func FormatSavings(tokens int) string {
	if tokens < 1000 {
		return strconv.Itoa(tokens)
	}
	thousands := float64(tokens) / 1000
	s := strings.Builder{}
	if thousands < 10 {
		whole := int(thousands)
		tenth := int(thousands*10) - whole*10
		s.WriteString(strconv.Itoa(whole))
		s.WriteByte('.')
		s.WriteString(strconv.Itoa(tenth))
	} else {
		s.WriteString(strconv.Itoa(int(thousands)))
	}
	s.WriteByte('k')
	return s.String()
}
