package web

import "testing"

// TestRedactURLUserinfo verifies credentials embedded in a URL-valued setting
// (e.g. semantics.base_url) are masked in the GET /api/settings response while
// the rest of the URL stays visible, and non-URL / credential-free values are
// untouched.
func TestRedactURLUserinfo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://user:pass@host:11434/v1", "http://redacted@host:11434/v1"},
		{"http://token@host/v1", "http://redacted@host/v1"},
		{"https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"", ""},
		{"voyage-code-3", "voyage-code-3"},
	}
	for _, c := range cases {
		if got := redactURLUserinfo(c.in); got != c.want {
			t.Errorf("redactURLUserinfo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
