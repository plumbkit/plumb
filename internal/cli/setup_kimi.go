package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/tools"
)

// Kimi Code's registration is the one that takes an option, so it lives here
// rather than in the generic setup_clients.go table.
//
// WHY A CLIENT-SIDE ALLOWLIST. plumb advertises its full 57-tool registry to
// Kimi Code, because Kimi is schema-discovery-only (internal/clientcaps): it
// builds its tool set purely from tools/list, with no deferred-schema mechanism,
// so plumb's own lean profile would make the hidden tools unreachable rather
// than merely undisplayed. The saving therefore has to be taken on the client
// side, where a filtered-out tool is a deliberate user choice rather than a
// capability plumb silently removed: Kimi's mcp.json supports a per-server
// "enabledTools" allowlist, and --lean writes tools.LeanToolNames() into it.
//
// PROVENANCE OF THE enabledTools CONTRACT. This is OBSERVED behaviour, recorded
// when Kimi Code support landed (2026-08-03) — not a contract quoted from
// published Kimi documentation, and there is no upstream reference to cite here.
// Treat it as unverified against any particular Kimi build. Two things make the
// assumption safe to ship on that footing:
//
//   - The failure mode is BENIGN and equals the status quo. If a Kimi build does
//     not implement the key, it is an unknown field on the server entry, which
//     JSON config readers ignore — Kimi then loads plumb's full advertised
//     surface, exactly what it does without --lean. Nothing plumb needs is
//     removed and no capability is lost; only the token saving fails to happen.
//   - Nothing plumb does depends on the key working. plumb never reads its own
//     effect: the profile Kimi is served is decided by clientcaps
//     (SchemaDiscoveryOnly ⇒ "full"), never by what the allowlist contains, so a
//     key that is ignored cannot desynchronise the server from the client.
//
// `plumb doctor` therefore grades the key's CONTENT (does it name registered
// tools, is it plumb's own aged snapshot) and never asserts an outcome it cannot
// observe — see kimiDegenerateAllowlistResult in doctor_allowlist.go, which
// hedges precisely where this contract is uncertain. If upstream documentation
// appears, cite it here and the hedging can tighten.

// setupKimiLeanFlag backs `plumb setup kimi-code --lean`. It is a package-level
// flag var like setupClaudeCodeProjectFlag, read at registration time by the
// Kimi target's intoFn — the bulk --all / --install-missing paths never set it,
// so they register bare and preserve whatever allowlist is already on disk.
var setupKimiLeanFlag bool

// registerKimiLeanFlag is the setupTarget.flags hook for Kimi Code.
func registerKimiLeanFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&setupKimiLeanFlag, "lean", false,
		"Also write a client-side enabledTools allowlist pinning plumb's lean tool set")
}

// kimiLeanNote is the setupTarget.note hook: it explains the allowlist's
// staleness contract, and only when --lean actually fired (a bare register
// prints nothing).
func kimiLeanNote() string {
	if !setupKimiLeanFlag {
		return ""
	}
	return fmt.Sprintf(
		"Tool allowlist: enabledTools now pins the %d lean plumb tools client-side.\n"+
			"It is a snapshot, not a live view — re-run `plumb setup kimi-code --lean` after a\n"+
			"plumb upgrade to refresh it. A later bare `plumb setup kimi-code` keeps the key;\n"+
			"delete it by hand to go back to the full tool surface.",
		len(tools.LeanToolNames()))
}

// kimiCodeInto registers plumb in Kimi Code's mcp.json (the plain mcpServers
// JSON shape, shared with Kimi Desktop). With lean set it additionally writes
// the "enabledTools" allowlist — tools.LeanToolNames(), the same set plumb's own
// lean profile advertises — so Kimi loads ~21 schemas instead of all 57.
//
// The idempotence predicate is lean-aware, and that is the subtle part. Without
// it, mergeServerEntry's "already points at this binary" short-circuit would
// make --lean a silent no-op on any machine where plumb was already registered:
// the entry matches on command, so nothing is written and the allowlist never
// appears. Requiring the existing allowlist to equal the wanted one when lean is
// requested defeats that. The resulting contract:
//
//   - fresh + --lean            → entry written with the allowlist
//   - registered + --lean       → allowlist added (short-circuit defeated)
//   - --lean twice              → no-op, no write, no backup churn
//   - --lean over a custom list → REPLACED (the flag means "pin plumb's set")
//   - later bare re-register    → allowlist PRESERVED (mergeServerEntry merges
//     the canonical fields onto the existing entry, the same contract that
//     keeps Codex's per-tool approval tables)
//
// There is deliberately no --full flag to unset the key: removing an allowlist
// is a one-line manual edit, and a flag that silently widens a surface the user
// narrowed on purpose is worse than none.
func kimiCodeInto(cfgPath, plumbBin string, lean bool) (added bool, preserved []string, err error) {
	entry := map[string]any{"command": plumbBin, "args": []string{"serve"}}
	// Built only under --lean. A bare register never writes the key and its
	// predicate never compares against it, and bare is the common case: every
	// client in a `plumb setup --all` sweep takes this path.
	var wantTools []string
	if lean {
		wantTools = tools.LeanToolNames()
		entry["enabledTools"] = wantTools
	}
	return mergeServerEntry(cfgPath, "mcpServers", readOrInitClaudeConfig, writeJSON, entry,
		func(existing map[string]any) bool {
			if existing["command"] != plumbBin {
				return false
			}
			return !lean || stringSliceEqual(existing["enabledTools"], wantTools)
		},
	)
}
