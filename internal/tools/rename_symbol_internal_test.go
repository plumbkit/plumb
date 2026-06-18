package tools

import "testing"

func TestIdentifierAt(t *testing.T) {
	tests := []struct {
		name string
		line string
		char int
		want string
	}{
		{"middle of token", "func Foo() {}", 6, "Foo"},
		{"start of token", "func Foo() {}", 5, "Foo"},
		{"just after token", "Foo()", 3, "Foo"},
		{"underscores and digits", "x = my_var2 + 1", 6, "my_var2"},
		{"leading spaces, no identifier", "   ", 0, ""},
		{"empty line", "", 0, ""},
		{"negative offset", "abc", -1, ""},
		{"offset past end after token", "abc", 3, "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := identifierAt(tt.line, tt.char); got != tt.want {
				t.Errorf("identifierAt(%q, %d) = %q, want %q", tt.line, tt.char, got, tt.want)
			}
		})
	}
}
