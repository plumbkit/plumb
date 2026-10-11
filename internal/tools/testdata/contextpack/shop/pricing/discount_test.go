package pricing

import "testing"

func TestApplyPercent(t *testing.T) {
	if got := Apply(1000, Discount{Percent: 10}); got != 900 {
		t.Fatalf("Apply = %d, want 900", got)
	}
}

// TestOffPercent calls Off on a local Discount, declared twice (the subtest
// re-declares d), so the call graph does not resolve the calls.
func TestOffPercent(t *testing.T) {
	var d Discount
	d.Percent = 10
	if got := d.Off(1000); got != 100 {
		t.Fatalf("Off = %d, want 100", got)
	}
	t.Run("zero", func(t *testing.T) {
		d := Discount{}
		if got := d.Off(1000); got != 0 {
			t.Fatalf("Off = %d, want 0", got)
		}
	})
}
