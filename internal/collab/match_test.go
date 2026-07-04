package collab

import "testing"

func TestMatchPath(t *testing.T) {
	tests := []struct {
		name  string
		globs []string
		path  string
		want  bool
	}{
		{"empty globs never match", nil, "internal/tools/ratelimit.go", false},
		{"prefix star full path", []string{"internal/tools/ratelimit*"}, "internal/tools/ratelimit.go", true},
		{"prefix star does not span dir", []string{"internal/tools/ratelimit*"}, "internal/tools/sub/ratelimit.go", false},
		{"slashless basename anywhere", []string{"*.go"}, "internal/tools/ratelimit.go", true},
		{"slashless exact basename", []string{"ratelimit.go"}, "cmd/ratelimit.go", true},
		{"dir doublestar prefix", []string{"internal/tools/**"}, "internal/tools/deep/nested.go", true},
		{"dir doublestar matches the dir itself", []string{"internal/tools/**"}, "internal/tools", true},
		{"dir doublestar excludes sibling", []string{"internal/tools/**"}, "internal/toolsx/a.go", false},
		{"no match", []string{"internal/auth/*.go"}, "internal/tools/ratelimit.go", false},
		{"one of several matches", []string{"docs/*.md", "internal/tools/ratelimit*"}, "internal/tools/ratelimit.go", true},
		{"whitespace trimmed", []string{"  internal/tools/ratelimit*  "}, "internal/tools/ratelimit.go", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchPath(tc.globs, tc.path); got != tc.want {
				t.Errorf("MatchPath(%v, %q) = %v, want %v", tc.globs, tc.path, got, tc.want)
			}
		})
	}
}
