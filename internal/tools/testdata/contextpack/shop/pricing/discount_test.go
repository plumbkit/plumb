package pricing

import "testing"

func TestApplyPercent(t *testing.T) {
	if got := Apply(1000, Discount{Percent: 10}); got != 900 {
		t.Fatalf("Apply = %d, want 900", got)
	}
}

func TestOffPercent(t *testing.T) {
	var d Discount
	d.Percent = 10
	if got := d.Off(1000); got != 100 {
		t.Fatalf("Off = %d, want 100", got)
	}
}
