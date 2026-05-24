package topology

import (
	"context"
	"testing"
)

// stubExtractor reports the patterns it claims so findExtractor can be tested
// without parsing anything.
type stubExtractor struct {
	lang string
	exts []string
}

func (s stubExtractor) Language() string     { return s.lang }
func (s stubExtractor) Extensions() []string { return s.exts }
func (s stubExtractor) Extract(context.Context, string, []byte) ([]Node, []Edge, error) {
	return nil, nil, nil
}

func TestFindExtractor_ExtensionAndBasename(t *testing.T) {
	go_ := stubExtractor{lang: "go", exts: []string{".go"}}
	docker := stubExtractor{lang: "dockerfile", exts: []string{"dockerfile", "containerfile"}}
	exts := []Extractor{go_, docker}
	cases := map[string]string{
		"main.go":              "go",
		"Dockerfile":           "dockerfile", // extensionless basename
		"build/Dockerfile":     "dockerfile",
		"Dockerfile.prod":      "dockerfile", // dotted prefix
		"service.dockerfile":   "dockerfile", // dotted suffix
		"Containerfile":        "dockerfile",
		"notes.txt":            "",   // no match
		"dockerfile_helper.go": "go", // basename stem must not match mid-word
	}
	for path, want := range cases {
		ex := findExtractor(path, exts)
		if want == "" {
			if ex != nil {
				t.Errorf("findExtractor(%q) = %q, want no match", path, ex.Language())
			}
			continue
		}
		if ex == nil || ex.Language() != want {
			got := "nil"
			if ex != nil {
				got = ex.Language()
			}
			t.Errorf("findExtractor(%q) = %q, want %q", path, got, want)
		}
	}
}

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
		if got := splitIdentifier(c.input); got != c.want {
			t.Errorf("splitIdentifier(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
