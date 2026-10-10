package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWithinMatches(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		// A literal selects itself and everything beneath it, on segment boundaries.
		{"cart", "cart/cart.go", true},
		{"cart", "cartridge/x.go", false},
		{"cart", "pkg/cart/x.go", false},
		{"cart/cart.go", "cart/cart.go", true},
		{"cart/cart.go", "cart/cart_test.go", false},
		{"internal/tools", "internal/tools/x.go", true},
		// A glob with a slash is anchored at the root and selects what lies below a match.
		{"cart/*", "cart/sub/x.go", true},
		{"pricing/*.go", "pricing/discount.go", true},
		{"pricing/*.go", "other/pricing/discount.go", false},
		{"pricing/**", "pricing/discount.go", true},
		// A glob without a slash matches one segment at any depth.
		{"*.go", "pricing/discount.go", true},
		{"*.go", "docs/pricing.md", false},
		{"x[ab].go", "pkg/xa.go", true},
		{"x[ab].go", "pkg/xc.go", false},
		// A leading **/ matches from any depth.
		{"**/discount.go", "a/b/discount.go", true},
		{"**/discount.go", "discount.go", true},
		{"**/discount.go", "a/b/other.go", false},
		{"**/vendor", "a/vendor/x.go", true},
		// A plain name with no slash is a literal from the root, not a segment match.
		{"test", "src/test/x.go", false},
		{"test", "test/x.go", true},
		// The whole root.
		{".", "anything/at/all.go", true},
	}
	for _, tc := range cases {
		if got := withinMatches(tc.pattern, tc.rel); got != tc.want {
			t.Errorf("withinMatches(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}

func TestContextScopeAllows(t *testing.T) {
	root := canonicalRoot(t.TempDir())
	outsideDir := t.TempDir()
	// A link inside the root that leads outside it: the path looks inside, the
	// file is not.
	link := filepath.Join(root, "link")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cases := []struct {
		name      string
		within    []string
		corpora   []string
		candidate string
		corpus    string
		want      bool
		reason    string
	}{
		{"no filters admit a root path", nil, nil, "a/b.go", corpusCode, true, ""},
		{"a relative climb leaves the root", nil, nil, "../x.go", corpusCode, false, "outside the workspace root"},
		{"the root itself names no file", nil, nil, ".", corpusCode, false, "outside the workspace root"},
		{"an absolute path inside is admitted", nil, nil, filepath.Join(root, "a", "b.go"), corpusCode, true, ""},
		{"an absolute path outside is not", nil, nil, filepath.Join(outsideDir, "x.go"), corpusCode, false, "outside the workspace root"},
		{"an unresolved dotdot is outside", nil, nil, root + "/a/../b.go", corpusCode, false, "outside the workspace root"},
		{"a symlink out of the root is outside", nil, nil, filepath.Join(link, "x.go"), corpusCode, false, "outside the workspace root"},
		{"corpora admits a listed corpus", nil, []string{corpusCode}, "a.go", corpusCode, true, ""},
		{"corpora refuses an unlisted one", nil, []string{corpusCode}, "a.md", corpusDocs, false, "corpus docs is not in corpora"},
		{"within admits a match", []string{"cart"}, nil, "cart/a.go", corpusCode, true, ""},
		{"within refuses a non-match", []string{"cart"}, nil, "pricing/a.go", corpusCode, false, "outside the within filter"},
		{"within entries are alternatives", []string{"cart", "pricing"}, nil, "pricing/a.go", corpusCode, true, ""},
		{"both filters must admit", []string{"cart"}, []string{corpusDocs}, "cart/a.go", corpusCode, false, "corpus code is not in corpora"},
		{"the root bound beats within", []string{"cart"}, nil, "../cart/a.go", corpusCode, false, "outside the workspace root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, err := newContextScope(root, tc.within, tc.corpora)
			if err != nil {
				t.Fatal(err)
			}
			got, reason := scope.allows(tc.candidate, tc.corpus)
			if got != tc.want || reason != tc.reason {
				t.Errorf("allows(%q, %s) = (%v, %q), want (%v, %q)", tc.candidate, tc.corpus, got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestNewContextScope(t *testing.T) {
	root := canonicalRoot(t.TempDir())
	outside := t.TempDir()
	for _, tc := range []struct {
		within  []string
		want    []string
		errPart string
	}{
		{within: []string{"./cart/"}, want: []string{"cart"}},
		{within: []string{"a/../b"}, want: []string{"b"}},
		{within: []string{filepath.Join(root, "pricing")}, want: []string{"pricing"}},
		{within: []string{"a/*.go", "**/b"}, want: []string{"a/*.go", "**/b"}},
		{within: []string{"  "}, errPart: "empty entry"},
		{within: []string{outside}, errPart: "outside the workspace root"},
		{within: []string{".."}, errPart: "reaches outside"},
		{within: []string{"a/../../b"}, errPart: "reaches outside"},
		{within: []string{"[x"}, errPart: "not a valid glob"},
	} {
		t.Run(strings.Join(tc.within, "+"), func(t *testing.T) {
			scope, err := newContextScope(root, tc.within, nil)
			if tc.errPart != "" {
				requireErr(t, err, tc.errPart)
				return
			}
			if err != nil {
				t.Fatalf("within %v: %v", tc.within, err)
			}
			if !slices.Equal(scope.within, tc.want) {
				t.Errorf("within %v normalised to %v, want %v", tc.within, scope.within, tc.want)
			}
		})
	}
}

func TestCorpusOfPath(t *testing.T) {
	for rel, want := range map[string]string{
		"cart/cart.go":                  corpusCode,
		"scripts/export.py":             corpusCode,
		"web/CartBadge.svelte":          corpusCode,
		"docs/pricing.md":               corpusDocs,
		"README.MD":                     corpusDocs,
		"notes/design.rst":              corpusDocs,
		"docs/example.go":               corpusCode, // a docs/ directory may hold code
		".plumb/memories/decision.md":   corpusMemory,
		".plumb/memories/nested/x.json": corpusMemory,
		".plumb/config.toml":            corpusCode,
	} {
		if got := corpusOfPath(rel); got != want {
			t.Errorf("corpusOfPath(%q) = %s, want %s", rel, got, want)
		}
	}
}

func TestSplitSymbolSeed(t *testing.T) {
	for _, tc := range []struct {
		in, path, selector, errPart string
	}{
		{in: "Total", selector: "Total"},
		{in: "  Total ", selector: "Total"},
		{in: "cart/cart.go#Cart.Add", path: "cart/cart.go", selector: "Cart.Add"},
		{in: "#Total", selector: "Total"},
		{in: "cart/cart.go#", errPart: "no selector after '#'"},
		{in: "docs/pricing.md#Totals", errPart: "names a document"},
		{in: "README.markdown#Intro", errPart: "corpora"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			path, selector, err := splitSymbolSeed(tc.in)
			if tc.errPart != "" {
				requireErr(t, err, tc.errPart)
				return
			}
			if err != nil || path != tc.path || selector != tc.selector {
				t.Errorf("splitSymbolSeed(%q) = (%q, %q, %v), want (%q, %q)", tc.in, path, selector, err, tc.path, tc.selector)
			}
		})
	}
}
