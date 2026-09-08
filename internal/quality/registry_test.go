package quality

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// The Settings pane prints a Tool's Language beside its name. That is only
// useful if it is the same language plumb uses everywhere else — a row saying
// "typescript" while langsupport calls it something different would be a label
// dressed up as a fact.
func TestRegistry_LanguagesExistInLangsupport(t *testing.T) {
	for _, tool := range Tools() {
		if _, ok := langsupport.ByName(tool.Language); !ok {
			t.Errorf("tool %q names language %q, which langsupport does not know",
				tool.Name, tool.Language)
		}
	}
}

func TestRegistry_NamesAreUniqueAndComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Tools() {
		switch {
		case tool.Name == "":
			t.Error("a registry row has no Name")
		case seen[tool.Name]:
			t.Errorf("duplicate registry name %q — ToolByName would resolve only the first", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Binary == "" {
			t.Errorf("tool %q has no Binary, so LookBinary can never resolve it", tool.Name)
		}
		if len(tool.Extensions) == 0 {
			t.Errorf("tool %q owns no extensions, so Supports is false for every file", tool.Name)
		}
	}
}

// A tool's extensions must actually belong to the language it declares: at
// least one of them has to resolve, through langsupport, to that language. A row
// pairing "python" with [".rs"] would otherwise sit in the registry advertising
// a language it never analyses, and the Settings pane would print the claim.
//
// It is an OVERLAP check rather than a subset one, deliberately. A tool may
// legitimately claim more than plumb's Map carries — ruff lints .pyi stubs and
// langsupport's python row does not list them — and the analyser's reach is the
// tool's business, not the indexer's. Requiring a subset would force a change to
// what plumb INDEXES in order to add a linter, which is not a trade this table
// should be able to demand.
func TestRegistry_ExtensionsMatchTheDeclaredLanguage(t *testing.T) {
	for _, tool := range Tools() {
		var matched bool
		for _, ext := range tool.Extensions {
			if lang, ok := langsupport.ByPath(probeName(ext)); ok && lang.Name == tool.Language {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("tool %q declares language %q but none of its extensions %v resolve to it",
				tool.Name, tool.Language, tool.Extensions)
		}
	}
}

// probeName turns a registry extension pattern into a filename langsupport.ByPath
// can classify: a dot-prefixed pattern needs a stem, a bare one ("dockerfile")
// IS the basename and must be passed through untouched — "xdockerfile" matches
// nothing.
func probeName(ext string) string {
	if strings.HasPrefix(ext, ".") {
		return "probe" + ext
	}
	return ext
}

func TestToolByName(t *testing.T) {
	if _, ok := ToolByName("ruff"); !ok {
		t.Error("ruff must be in the registry — it is one of the two implemented adapters")
	}
	// Exact match only. A near-miss resolving to a different tool would turn a
	// typo into a silently wrong analyser, which is worse than a rejected entry.
	for _, miss := range []string{"Ruff", "ruff ", "ruff-check", "/usr/bin/ruff", ""} {
		if _, ok := ToolByName(miss); ok {
			t.Errorf("ToolByName(%q) matched; the lookup must be exact", miss)
		}
	}
}

func TestImplementedNames(t *testing.T) {
	got := ImplementedNames()
	want := map[string]bool{"golangci-lint": true, "ruff": true}
	if len(got) != len(want) {
		t.Fatalf("ImplementedNames() = %v, want exactly %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("ImplementedNames() includes %q, which has no adapter", n)
		}
	}
}

func TestTool_SupportsPath(t *testing.T) {
	ruff := mustTool(t, "ruff")
	golangci := mustTool(t, "golangci-lint")
	hadolint := mustTool(t, "hadolint")

	cases := []struct {
		tool Tool
		path string
		want bool
	}{
		{ruff, "/project/app.py", true},
		{ruff, "/project/stubs.pyi", true},
		{ruff, "/project/app.PY", true}, // extension match is case-insensitive
		{ruff, "/project/main.go", false},
		{ruff, "/project/app.py.bak", false},
		{golangci, "/project/main.go", true},
		{golangci, "/project/app.py", false},
		{golangci, "/project/noext", false},
		// A bare pattern names an extensionless file, matched the way
		// langsupport.MatchExtPattern matches one.
		{hadolint, "/project/Dockerfile", true},
		{hadolint, "/project/Dockerfile.prod", true},
		{hadolint, "/project/prod.dockerfile", true},
		{hadolint, "/project/main.go", false},
	}
	for _, tc := range cases {
		if got := tc.tool.SupportsPath(tc.path); got != tc.want {
			t.Errorf("%s.SupportsPath(%q) = %v, want %v", tc.tool.Name, tc.path, got, tc.want)
		}
	}
}
