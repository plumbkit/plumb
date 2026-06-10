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

// TestSplitIdentifier moved to internal/tokenise — the canonical tokeniser now
// lives there, shared by the topology and memory indexers.
