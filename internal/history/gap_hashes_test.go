package history

import "testing"

// Each side's "never hashed" clause decides alone: in the rows below where it
// matters, the other side is absent (a nil hash that IS known), so the byte
// comparison would call two nil hashes equal and hide the unknown.
func TestHashesDiffer(t *testing.T) {
	h, g := []byte{1}, []byte{2}
	for _, tc := range []struct {
		name         string
		prev         []byte
		prevExists   bool
		before       []byte
		beforeExists bool
		want         bool
	}{
		{"known and equal: continuous", h, true, h, true, false},
		{"known and different: a gap", h, true, g, true, true},
		{"both absent: continuous", nil, false, nil, false, false},
		{"previous side unhashed, next absent: unknown, so a gap", nil, true, nil, false, true},
		{"previous absent, next side unhashed: unknown, so a gap", nil, false, nil, true, true},
		{"both unhashed: unknown, so a gap", nil, true, nil, true, true},
	} {
		if got := hashesDiffer(tc.prev, tc.prevExists, tc.before, tc.beforeExists); got != tc.want {
			t.Errorf("%s: hashesDiffer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
