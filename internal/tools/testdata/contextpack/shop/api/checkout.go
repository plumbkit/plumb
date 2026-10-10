// Package api exposes the checkout entry point.
package api

import (
	"example.com/shop/cart"
	"example.com/shop/store"
)

// Checkout prices the cart and records the charge. Its calls are receiver
// method calls, which the syntactic call graph does not resolve.
func Checkout(c *cart.Cart, l *store.Ledger) int {
	t := c.Total()
	l.Record(t)
	return t
}
