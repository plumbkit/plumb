package pricing

// Savings is what d takes off subtotal. It calls Apply by its bare name from a
// sibling file, a call the syntactic call graph never resolves.
func Savings(subtotal int, d Discount) int { return subtotal - Apply(subtotal, d) }
