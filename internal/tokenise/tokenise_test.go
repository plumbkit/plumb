package tokenise

import "testing"

// TestSplitIdentifier is the canonical table for the shared tokeniser. The
// topology and memory FTS indexers both rely on this exact behaviour, so this
// test moving here (from internal/topology) is the single source of truth.
func TestSplitIdentifier(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"foo", "foo"},
		{"Foo", "foo"},
		{"fooBar", "foo bar"},
		{"FooBar", "foo bar"},
		{"workspacePool", "workspace pool"},
		{"HandleRequest", "handle request"},
		// Acronym boundary: HTTPServer → http server
		{"HTTPServer", "http server"},
		// Acronym boundary: parseHTTPRequest → parse http request
		{"parseHTTPRequest", "parse http request"},
		// All uppercase acronym: HTTP → http (single token)
		{"HTTP", "http"},
		// snake_case
		{"foo_bar", "foo bar"},
		// kebab-case
		{"foo-bar", "foo bar"},
		// dot-separated (package paths)
		{"foo.bar", "foo bar"},
		// slash-separated
		{"foo/bar", "foo bar"},
		// mixed: camel + underscore
		{"fooBar_baz", "foo bar baz"},
		// consecutive acronym mid-word: XMLParser
		{"XMLParser", "xml parser"},
	}
	for _, c := range cases {
		if got := SplitIdentifier(c.input); got != c.want {
			t.Errorf("SplitIdentifier(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
