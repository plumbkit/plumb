// Package store records completed checkouts.
package store

// Ledger accumulates charged totals.
type Ledger struct {
	entries []int
}

// Record appends one charged amount.
func (l *Ledger) Record(cents int) { l.entries = append(l.entries, cents) }

// Total is the sum of every recorded charge. It shares its name with
// cart.(*Cart).Total on purpose: a bare "Total" selector is ambiguous.
func (l *Ledger) Total() int {
	sum := 0
	for _, e := range l.entries {
		sum += e
	}
	return sum
}
