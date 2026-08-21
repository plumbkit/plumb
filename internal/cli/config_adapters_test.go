package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestNormaliseAdapterFlag(t *testing.T) {
	cases := map[string]string{
		"adapters":     "adapters",
		"adapter":      "adapters",
		"lsp":          "adapters",
		"lsps":         "adapters",
		"integration":  "adapters",
		"integrations": "adapters",
		"workspace":    "workspace", // unrelated flag passes through unchanged
	}
	for in, want := range cases {
		if got := string(normaliseAdapterFlag(nil, in)); got != want {
			t.Errorf("normaliseAdapterFlag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdapterMetaFor(t *testing.T) {
	if m := adapterMetaFor("go"); m.display != "Go" || m.tier != tierFirstClass {
		t.Errorf("go meta = %+v, want Go/first-class", m)
	}
	if m := adapterMetaFor("html"); m.display != "HTML" || m.tier != tierExperimental {
		t.Errorf("html meta = %+v, want HTML/experimental", m)
	}
	// Unknown key falls back to a title-cased name at the experimental tier.
	if m := adapterMetaFor("nim"); m.display != "Nim" || m.tier != tierExperimental {
		t.Errorf("unknown meta = %+v, want Nim/experimental", m)
	}
}

// TestAdapterCatalogueTiersMatchTheDocumentedStatus pins every tier, because
// this table is the one place a promotion is easy to miss: typescript and zig
// were promoted to Validated in docs/adding-an-lsp.md, README.md, roadmap.md
// and their doc.go files, and stayed experimental here for months — so
// `plumb config show --adapters` under-reported them. If you change a tier,
// change it in all of those places (the promotion checklist in
// docs/adding-an-lsp.md now names this file).
func TestAdapterCatalogueTiersMatchTheDocumentedStatus(t *testing.T) {
	want := map[string]adapterTier{
		"go":         tierFirstClass,
		"python":     tierFirstClass,
		"java":       tierValidated,
		"rust":       tierValidated,
		"swift":      tierValidated,
		"typescript": tierValidated,
		"zig":        tierValidated,
		"kotlin":     tierValidated,
		"html":       tierExperimental,
	}
	if len(adapterCatalogue) != len(want) {
		t.Fatalf("catalogue has %d entries, want %d — add the new adapter to this table too", len(adapterCatalogue), len(want))
	}
	lastTier := tierFirstClass
	for _, e := range adapterCatalogue {
		wantTier, known := want[e.key]
		if !known {
			t.Errorf("uncatalogued key %q — add it to this test's table", e.key)
			continue
		}
		if e.meta.tier != wantTier {
			t.Errorf("%s tier = %s, want %s", e.key, e.meta.tier.label(), wantTier.label())
		}
		// The slice order drives the rendered table order, which is documented
		// as tier-grouped.
		if e.meta.tier < lastTier {
			t.Errorf("%s (%s) breaks the tier grouping — it follows a %s entry", e.key, e.meta.tier.label(), lastTier.label())
		}
		lastTier = e.meta.tier
	}
}

func TestAdapterOrder(t *testing.T) {
	lsp := map[string]config.LSPConfig{
		"zig":    {},
		"go":     {},
		"nim":    {}, // uncatalogued — sorts after catalogued keys
		"alpine": {}, // uncatalogued — alphabetical among extras
		"python": {},
	}
	got := adapterOrder(lsp)
	want := []string{"go", "python", "zig", "alpine", "nim"}
	if len(got) != len(want) {
		t.Fatalf("adapterOrder len = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("adapterOrder = %v, want %v", got, want)
		}
	}
}

func TestRenderAdapterActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.LSPConfig
		want string
	}{
		{"disabled", config.LSPConfig{Enabled: false, Command: "gopls"}, "disabled"},
		{"install-gated", config.LSPConfig{Enabled: true, Command: "definitely-not-on-path-xyz"}, "install-gated"},
	}
	for _, c := range cases {
		got := stripANSI(renderAdapterActive(c.cfg))
		if got != c.want {
			t.Errorf("%s: renderAdapterActive = %q, want %q", c.name, got, c.want)
		}
	}
}
