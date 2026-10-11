package cart

import "testing"

// The tests call methods on a local Cart that is declared twice (each subtest
// re-declares c), so the call graph does not resolve the calls.

func TestAddMergesRepeatedSKU(t *testing.T) {
	var c Cart
	c.Add(Item{SKU: "a", Cents: 100, Qty: 1})
	c.Add(Item{SKU: "a", Cents: 100, Qty: 2})
	if got := c.Total(); got != 300 {
		t.Fatalf("Total = %d, want 300", got)
	}
	t.Run("empty", func(t *testing.T) {
		var c Cart
		if got := c.Total(); got != 0 {
			t.Fatalf("Total = %d, want 0", got)
		}
	})
}

func TestTotalAppliesPercentCode(t *testing.T) {
	var c Cart
	c.Add(Item{SKU: "a", Cents: 1000, Qty: 1})
	c.ApplyCode("TENOFF")
	if got := c.Total(); got != 900 {
		t.Fatalf("Total = %d, want 900", got)
	}
	t.Run("no code", func(t *testing.T) {
		c := Cart{}
		c.Add(Item{SKU: "a", Cents: 1000, Qty: 1})
		if got := c.Total(); got != 1000 {
			t.Fatalf("Total = %d, want 1000", got)
		}
	})
}
