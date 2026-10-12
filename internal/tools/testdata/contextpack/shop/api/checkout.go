// Package api exposes the checkout entry point.
package api

import (
	"example.com/shop/cart"
	"example.com/shop/store"
)

// Checkout prices the cart and records the charge. Its calls are method calls on
// plain copies of its parameters (cc := c), which the call graph does not resolve.
func Checkout(c *cart.Cart, l *store.Ledger) int {
	cc, ll := c, l
	t := cc.Total()
	ll.Record(t)
	return t
}
