// Package pricing turns discount codes into reductions.
package pricing

// Discount is a percentage or fixed reduction.
type Discount struct {
	Percent    int
	FixedCents int
}

var codes = map[string]Discount{
	"TENOFF": {Percent: 10},
	"FIVER":  {FixedCents: 500},
}

// Lookup returns the discount for code, or the zero Discount.
func Lookup(code string) Discount { return codes[code] }

// Apply reduces subtotal by d.
func Apply(subtotal int, d Discount) int {
	return subtotal - subtotal*d.Percent/100 - d.FixedCents
}
