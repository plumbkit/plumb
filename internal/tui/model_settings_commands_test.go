package tui

import (
	"reflect"
	"testing"
)

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"go build ./...", []string{"go", "build", "./..."}},
		{"go test -run 'Test Foo' ./...", []string{"go", "test", "-run", "Test Foo", "./..."}},
		{`echo "a b" c`, []string{"echo", "a b", "c"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"", nil},
	}
	for _, tc := range cases {
		if got := shellSplit(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shellSplit(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// TestShellSplitJoinRoundTrip proves the exec editor round-trips an argv through
// its display/parse pair — the whole point of adding quoting is that an argument
// with a space survives an edit.
func TestShellSplitJoinRoundTrip(t *testing.T) {
	argvs := [][]string{
		{"go", "test", "-run", "Test Foo", "./..."},
		{"golangci-lint", "run"},
		{"echo", "a'b"},
		{"echo", "a b c"},
		{"echo", `a"b`},
		{"echo", "a 'quoted b"},
	}
	for _, argv := range argvs {
		joined := shellJoin(argv)
		if got := shellSplit(joined); !reflect.DeepEqual(got, argv) {
			t.Errorf("round-trip failed: %#v → %q → %#v", argv, joined, got)
		}
	}
}
