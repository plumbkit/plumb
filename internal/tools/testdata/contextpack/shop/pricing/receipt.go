package pricing

// Savings is what d takes off subtotal. It calls Apply by its bare name from a
// sibling file, a call the call graph resolves.
func Savings(subtotal int, d Discount) int { return subtotal - Apply(subtotal, d) }

// Tenth is what a ten percent discount takes off subtotal. It calls Off on a local
// Discount, a method call the call graph does not resolve: only the local
// declaration shows the receiver is a Discount.
func Tenth(subtotal int) int {
	var d Discount
	d.Percent = 10
	return d.Off(subtotal)
}
