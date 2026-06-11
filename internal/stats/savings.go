package stats

import "strings"

// FormatSavings renders a token count as a short human string ("1.2k", "850").
func FormatSavings(tokens int) string {
	if tokens < 1000 {
		return itoa(tokens)
	}
	thousands := float64(tokens) / 1000
	s := strings.Builder{}
	if thousands < 10 {
		whole := int(thousands)
		tenth := int(thousands*10) - whole*10
		s.WriteString(itoa(whole))
		s.WriteByte('.')
		s.WriteString(itoa(tenth))
	} else {
		s.WriteString(itoa(int(thousands)))
	}
	s.WriteByte('k')
	return s.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
