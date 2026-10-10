// Package cart holds a shopper's line items and prices them.
package cart

import "example.com/shop/pricing"

// Cart holds line items for one checkout.
type Cart struct {
	items []Item
	code  string
}

// Item is one priced line.
type Item struct {
	SKU   string
	Cents int
	Qty   int
}

// Add appends an item, merging quantities for a repeated SKU.
func (c *Cart) Add(it Item) {
	for i := range c.items {
		if c.items[i].SKU == it.SKU {
			c.items[i].Qty += it.Qty
			return
		}
	}
	c.items = append(c.items, it)
}

// Remove drops every line for sku.
func (c *Cart) Remove(sku string) {
	kept := c.items[:0]
	for _, it := range c.items {
		if it.SKU != sku {
			kept = append(kept, it)
		}
	}
	c.items = kept
}

// ApplyCode records a discount code that Total will honour.
func (c *Cart) ApplyCode(code string) { c.code = code }

// Total returns the payable amount in cents after any discount.
func (c *Cart) Total() int {
	sub := 0
	for _, it := range c.items {
		sub += it.Cents * it.Qty
	}
	return pricing.Apply(sub, pricing.Lookup(c.code))
}
