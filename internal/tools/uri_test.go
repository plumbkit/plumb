package tools

import "testing"

func TestToFileURI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"absolute path gains scheme", "/abs/path.go", "file:///abs/path.go"},
		{"file uri unchanged", "file:///abs/path.go", "file:///abs/path.go"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toFileURI(tc.in); got != tc.want {
				t.Errorf("toFileURI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Round-trip: stripping file:// then re-adding is idempotent.
	if got := toFileURI(toFileURI("/x.go")); got != "file:///x.go" {
		t.Errorf("toFileURI is not idempotent: got %q", got)
	}
}
