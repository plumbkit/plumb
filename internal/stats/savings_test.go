package stats

import "testing"

func TestFormatSavings(t *testing.T) {
	cases := []struct {
		tokens int
		want   string
	}{
		{0, "0"},
		{850, "850"},
		{999, "999"},
		{1000, "1.0k"},
		{1234, "1.2k"},
		{9990, "9.9k"},
		{10000, "10k"},
		{298000, "298k"},
	}
	for _, tc := range cases {
		if got := FormatSavings(tc.tokens); got != tc.want {
			t.Errorf("FormatSavings(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
}
