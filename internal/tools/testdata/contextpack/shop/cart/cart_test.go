package cart

import "testing"

func TestAddMergesRepeatedSKU(t *testing.T) {
	var c Cart
	c.Add(Item{SKU: "a", Cents: 100, Qty: 1})
	c.Add(Item{SKU: "a", Cents: 100, Qty: 2})
	if got := c.Total(); got != 300 {
		t.Fatalf("Total = %d, want 300", got)
	}
}

func TestTotalAppliesPercentCode(t *testing.T) {
	var c Cart
	c.Add(Item{SKU: "a", Cents: 1000, Qty: 1})
	c.ApplyCode("TENOFF")
	if got := c.Total(); got != 900 {
		t.Fatalf("Total = %d, want 900", got)
	}
}
