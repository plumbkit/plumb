package pricing

import "testing"

func TestApplyPercent(t *testing.T) {
	if got := Apply(1000, Discount{Percent: 10}); got != 900 {
		t.Fatalf("Apply = %d, want 900", got)
	}
}
