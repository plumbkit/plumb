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
	if m := adapterMetaFor("zig"); m.display != "Zig" || m.tier != tierExperimental {
		t.Errorf("zig meta = %+v, want Zig/experimental", m)
	}
	// Unknown key falls back to a title-cased name at the experimental tier.
	if m := adapterMetaFor("nim"); m.display != "Nim" || m.tier != tierExperimental {
		t.Errorf("unknown meta = %+v, want Nim/experimental", m)
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
