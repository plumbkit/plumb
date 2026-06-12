package cli

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/pflag"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tui"
)

// adapterTier classifies how thoroughly a language-server adapter has been
// exercised. It is presentation metadata for `plumb config show --adapters`,
// mirroring the "Adapter validation status" table in AGENTS.md.
type adapterTier int

const (
	tierFirstClass adapterTier = iota
	tierValidated
	tierExperimental
)

func (t adapterTier) label() string {
	switch t {
	case tierFirstClass:
		return "first-class"
	case tierValidated:
		return "validated"
	default:
		return "experimental"
	}
}

// adapterMeta is the per-adapter display metadata, keyed by the [lsp.<key>]
// config key.
type adapterMeta struct {
	display string
	tier    adapterTier
}

// adapterCatalogue lists each known language-server config key with its display
// name and validation tier. The slice order drives the table order (tier-grouped:
// first-class, then validated, then experimental). A [lsp.*] key absent here
// falls back to the title-cased key at the experimental tier, so a user-added
// adapter still renders.
var adapterCatalogue = []struct {
	key  string
	meta adapterMeta
}{
	{"go", adapterMeta{"Go", tierFirstClass}},
	{"python", adapterMeta{"Python", tierFirstClass}},
	{"java", adapterMeta{"Java", tierValidated}},
	{"rust", adapterMeta{"Rust", tierValidated}},
	{"swift", adapterMeta{"Swift", tierValidated}},
	{"typescript", adapterMeta{"TS/JS", tierExperimental}},
	{"zig", adapterMeta{"Zig", tierExperimental}},
	{"kotlin", adapterMeta{"Kotlin", tierExperimental}},
	{"html", adapterMeta{"HTML", tierExperimental}},
}

// adapterAliasFlags are the alternative spellings of the --adapters flag,
// normalised onto the canonical name so one flag answers to all of them.
var adapterAliasFlags = map[string]string{
	"adapter":      "adapters",
	"lsp":          "adapters",
	"lsps":         "adapters",
	"integration":  "adapters",
	"integrations": "adapters",
}

// normaliseAdapterFlag maps the --adapters aliases onto the canonical flag name.
// Applied to every flag on `config show`, so unrelated flags pass through
// unchanged.
func normaliseAdapterFlag(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	if canonical, ok := adapterAliasFlags[name]; ok {
		return pflag.NormalizedName(canonical)
	}
	return pflag.NormalizedName(name)
}

// adapterMetaFor returns the display metadata for a config key, falling back to
// the title-cased key at the experimental tier for unknown keys.
func adapterMetaFor(key string) adapterMeta {
	for _, e := range adapterCatalogue {
		if e.key == key {
			return e.meta
		}
	}
	return adapterMeta{display: titleKey(key), tier: tierExperimental}
}

func titleKey(key string) string {
	if key == "" {
		return key
	}
	return strings.ToUpper(key[:1]) + key[1:]
}

// adapterOrder returns the LSP config keys in catalogue order, with any
// uncatalogued keys appended alphabetically.
func adapterOrder(lsp map[string]config.LSPConfig) []string {
	seen := make(map[string]bool, len(lsp))
	order := make([]string, 0, len(lsp))
	for _, e := range adapterCatalogue {
		if _, ok := lsp[e.key]; ok {
			order = append(order, e.key)
			seen[e.key] = true
		}
	}
	extra := make([]string, 0)
	for key := range lsp {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// printAdaptersView renders the focused language-server adapter table for
// `plumb config show --adapters`.
func printAdaptersView(cfg config.Config) {
	fmt.Printf("Language Server Adapters\n")

	t := configShowTableBase().
		Headers("Language", "Server", "Tier", "Active").
		StyleFunc(configShowColStyle())

	active := 0
	for _, key := range adapterOrder(cfg.LSP) {
		lspCfg := cfg.LSP[key]
		if lspCfg.Command == "" {
			continue
		}
		meta := adapterMetaFor(key)
		if lspActive(lspCfg) {
			active++
		}
		t.Row(meta.display, lspCfg.Command, renderTier(meta.tier), renderAdapterActive(lspCfg))
	}

	fmt.Println(renderConfigShowTable(t))
	fmt.Println(adapterLegend(active))
	fmt.Println()
}

func renderTier(tier adapterTier) string {
	switch tier {
	case tierExperimental:
		return tui.WarnStyle.Render(tier.label())
	default:
		return tui.OkStyle.Render(tier.label())
	}
}

// renderAdapterActive reduces an LSP config to a one-word activation state:
// ready (enabled + installed), install-gated (enabled, binary absent), or
// disabled (excluded in config).
func renderAdapterActive(cfg config.LSPConfig) string {
	switch {
	case !cfg.Enabled:
		return tui.WarnStyle.Render("disabled")
	case !lspInstalled(cfg.Command):
		return tui.MutedStyle.Render("install-gated")
	default:
		return tui.OkStyle.Render("● ready")
	}
}

func adapterLegend(active int) string {
	noun := "adapters"
	if active == 1 {
		noun = "adapter"
	}
	return tui.MutedStyle.Render(fmt.Sprintf(
		"%d active %s · on PATH = active · set [lsp.<lang>] enabled = false to exclude one",
		active, noun))
}

// configShowColStyle returns a StyleFunc for a config-show table that pads every
// cell, renders the header row in HintStyle, and centre-aligns the given 0-based
// columns. The ✓/✗ status columns are centred so the marks sit under their
// header rather than hugging the left edge.
func configShowColStyle(centred ...int) func(row, col int) lipgloss.Style {
	set := make(map[int]struct{}, len(centred))
	for _, c := range centred {
		set[c] = struct{}{}
	}
	return func(row, col int) lipgloss.Style {
		s := lipgloss.NewStyle().Padding(0, 1)
		if _, ok := set[col]; ok {
			s = s.Align(lipgloss.Center)
		}
		if row == table.HeaderRow {
			return s.Inherit(tui.HintStyle)
		}
		return s
	}
}
