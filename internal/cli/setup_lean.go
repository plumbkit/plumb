package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/tools"
)

// leanClient describes one client whose OWN MCP config can carry a plumb tool
// allowlist — the client-side half of the lean tool surface, written by
// `plumb setup <client> --lean` and graded by `plumb doctor`.
//
// WHY A CLIENT-SIDE ALLOWLIST. plumb advertises its full tool registry to these
// clients because none of them carries a verified deferred-discovery capability
// (internal/clientcaps): they build their tool set from tools/list, so plumb's
// own lean profile would make a hidden tool unreachable rather than merely
// undisplayed. The saving therefore has to be taken on the client side, where a
// filtered-out tool is a deliberate user choice rather than a capability plumb
// silently removed.
//
// PROVENANCE OF EACH KEY. The allowlist key is the client's, not plumb's, and
// each is recorded with the evidence it was added on (see the per-client
// descriptors below). The failure mode is BENIGN and equals the status quo in
// every case: a build that does not implement its key sees an unknown field on
// the server entry, ignores it, and loads plumb's full advertised surface —
// exactly what it does without --lean. Nothing plumb does depends on the key
// working either: the profile a client is served is decided by clientcaps, never
// by what the allowlist contains, so a key that is ignored cannot desynchronise
// the server from the client. `plumb doctor` therefore grades the key's CONTENT
// (does it name registered tools, is it plumb's own aged snapshot) and never
// asserts an outcome it cannot observe — see doctor_allowlist.go.
type leanClient struct {
	setupCmd   string // the `plumb setup <cmd>` subcommand that manages the key
	name       string // display name, for doctor lines and hints
	key        string // the per-server allowlist key in the client's own config
	serversKey string // the config key holding the client's MCP server map
	// bareNote closes the --lean hint by saying what a later bare re-register
	// does to the key. It is per-client because the contracts differ — see
	// kimiCodeInto (preserves) against codexLeanInto/geminiLeanInto (clear).
	bareNote string
	// bareClears records which of the two contracts this client is on: true when
	// a bare named re-register DELETES the key (Codex, Gemini CLI), false when it
	// preserves it (Kimi Code, which shipped first with no clearing path). It is
	// data rather than prose because repointFix has to reason about it — the
	// safety of suggesting `--lean` depends on what the alternative would do.
	bareClears bool
	pathFn     func() (string, error)
	// read/write are the setup-side serialisation pair handed to
	// mergeServerEntry; read creates the parent directory for an absent config.
	read  func(string) (map[string]any, bool, error)
	write func(string, map[string]any) error
	// parse is the doctor-side reader: it decodes an existing config and creates
	// NOTHING, because a check must never write to the filesystem it inspects.
	parse func(string) (map[string]any, error)
}

// checkName is the client's `plumb doctor` check label.
func (c leanClient) checkName() string { return c.name + " (tool surface)" }

// leanCmd renders the command that writes this client's allowlist.
func (c leanClient) leanCmd() string { return "`plumb setup " + c.setupCmd + " --lean`" }

// The three clients that take --lean today. Each key is the client's own, and
// none of them supports globbing — the value is always exact tool names.
var (
	// Kimi Code: "enabledTools" on the plumb entry of its mcp.json. OBSERVED
	// behaviour recorded when Kimi Code support landed (2026-08-03), not a
	// contract quoted from published documentation; treat it as unverified
	// against any particular Kimi build.
	kimiLeanClient = leanClient{
		setupCmd: "kimi-code", name: "Kimi Code", key: "enabledTools", serversKey: "mcpServers",
		bareNote: "A later bare `plumb setup kimi-code` keeps the key;\n" +
			"delete it by hand to go back to the full tool surface.",
		pathFn: KimiCodeConfigPath, read: readOrInitClaudeConfig, write: writeJSON, parse: parseJSONConfig,
	}
	// Codex: "enabled_tools" on [mcp_servers.plumb] in config.toml, a TOML array
	// of exact tool names with no globbing (openai/codex#5367).
	codexLeanClient = leanClient{
		setupCmd: "codex", name: "Codex", key: "enabled_tools", serversKey: "mcp_servers",
		bareNote: "A later bare `plumb setup codex`\n" +
			"clears the key and restores the full tool surface; the bulk\n" +
			"`plumb setup --all`/`--repair` sweeps preserve it.",
		bareClears: true,
		pathFn:     CodexConfigPath, read: readOrInitCodexConfig, write: writeTOML, parse: parseTOMLConfig,
	}
	// Gemini CLI: "includeTools" on mcpServers.plumb in settings.json — a
	// list-time filter over exact names, no globbing
	// (google-gemini/gemini-cli#2976). Its sibling "excludeTools" wins wherever
	// both are present, which is why the note says so; plumb only ever writes
	// includeTools, and never touches the deprecated global tools.allowed /
	// tools.exclude settings.
	geminiLeanClient = leanClient{
		setupCmd: "gemini", name: "Gemini CLI", key: "includeTools", serversKey: "mcpServers",
		bareNote: "A later bare `plumb setup gemini`\n" +
			"clears the key and restores the full tool surface; the bulk\n" +
			"`plumb setup --all`/`--repair` sweeps preserve it. Gemini's\n" +
			"`excludeTools` wins wherever both keys are present.",
		bareClears: true,
		pathFn:     GeminiConfigPath, read: readOrInitClaudeConfig, write: writeJSON, parse: parseJSONConfig,
	}
)

// leanAllowlistClients is the set `plumb doctor` grades, in display order.
func leanAllowlistClients() []leanClient {
	return []leanClient{kimiLeanClient, codexLeanClient, geminiLeanClient}
}

// leanClientFor resolves a `plumb setup` subcommand name to its allowlist
// descriptor. ok is false for the ten clients that have no allowlist key.
func leanClientFor(use string) (leanClient, bool) {
	for _, c := range leanAllowlistClients() {
		if c.setupCmd == use {
			return c, true
		}
	}
	return leanClient{}, false
}

// leanChoice is what one registration should do with the client-side allowlist.
type leanChoice int

const (
	// leanKeep leaves whatever is on disk untouched. It is what the bulk
	// `plumb setup --all`/`--repair` sweeps use: they carry no --lean state at
	// all, so treating their silence as "no allowlist wanted" would silently
	// widen a tool surface the user narrowed on purpose.
	leanKeep leanChoice = iota
	// leanPin writes tools.LeanToolNames() — the ONLY permitted source. A
	// client-side allowlist is enforced by the CLIENT, so plumb's server-side
	// "bootstrap tools are always advertised" guarantee cannot rescue a
	// bootstrap tool the client itself filtered out.
	leanPin
	// leanClear removes the key, restoring the client's full plumb surface.
	leanClear
)

// leanChoiceOf maps a named subcommand's --lean flag to a choice: the flag state
// on the command line is authoritative for that client, so bare means "the
// default full surface" and the key goes. A bulk sweep never has an
// authoritative flag state, hence leanKeep.
//
// Kimi Code deliberately does NOT use this — its shipped contract preserves the
// key on a bare re-register (see kimiCodeInto).
func leanChoiceOf(lean bool) leanChoice {
	switch {
	case bulkSetupRunning():
		return leanKeep
	case lean:
		return leanPin
	default:
		return leanClear
	}
}

// mergeLeanEntry is the shared body of every --lean-capable client's writer: the
// same merge and the same lean-aware idempotence, differing only in the config
// format, the server key, the allowlist key, and the base-entry predicate.
//
// The lean-aware predicate is the subtle part. Without it, mergeServerEntry's
// "already points at this binary" short-circuit would make --lean a silent no-op
// on any machine where plumb is already registered: the entry matches on
// command, nothing is written, and the allowlist never appears. Requiring the
// existing key to already match the requested choice defeats that, and gives:
//
//   - fresh + --lean            → entry written with the allowlist
//   - registered + --lean       → allowlist added (short-circuit defeated)
//   - --lean twice              → no-op, no write, no backup churn
//   - --lean over a custom list → REPLACED (the flag means "pin plumb's set")
func mergeLeanEntry(
	c leanClient, cfgPath string, entry map[string]any, choice leanChoice,
	sameBase func(existing map[string]any) bool,
) (added bool, preserved []string, err error) {
	want := applyLeanChoice(entry, c.key, choice)
	return mergeServerEntry(cfgPath, c.serversKey, c.read, c.write, entry,
		func(existing map[string]any) bool {
			return sameBase(existing) && leanAllowlistCurrent(existing, c.key, choice, want)
		},
	)
}

// applyLeanChoice stamps the client's allowlist key onto a server entry per
// choice, returning the names --lean pins (nil for the other two choices).
// leanClear uses removeKey, mergeServerEntry's "delete this key" sentinel —
// deletion is the one thing a merge cannot otherwise express.
func applyLeanChoice(entry map[string]any, key string, choice leanChoice) []string {
	switch choice {
	case leanPin:
		names := tools.LeanToolNames()
		entry[key] = names
		return names
	case leanClear:
		entry[key] = removeKey{}
	case leanKeep:
	}
	return nil
}

// leanAllowlistCurrent reports whether an existing server entry already matches
// choice — the lean-aware half of mergeLeanEntry's idempotence predicate.
func leanAllowlistCurrent(existing map[string]any, key string, choice leanChoice, want []string) bool {
	switch choice {
	case leanPin:
		return stringSliceEqual(existing[key], want)
	case leanClear:
		_, has := existing[key]
		return !has
	default: // leanKeep
		return true
	}
}

// setupCodexLeanFlag and setupGeminiLeanFlag back `--lean` on their named
// subcommands, like setupKimiLeanFlag. They are read at call time by the
// targets' intoFns, so the bulk --all/--install-missing paths (which never set
// them, and which bulkSetupRunning detects) leave an existing allowlist alone.
var (
	setupCodexLeanFlag  bool
	setupGeminiLeanFlag bool
)

// leanFlagRegistrar builds the setupTarget.flags hook for a --lean-capable
// client, binding the flag to the var that client's intoFn reads.
func leanFlagRegistrar(v *bool, c leanClient) func(*cobra.Command) {
	return func(cmd *cobra.Command) {
		cmd.Flags().BoolVar(v, "lean", false,
			fmt.Sprintf("Also write a client-side %s allowlist pinning plumb's lean tool set", c.key))
	}
}

// leanSetupNote is the setupTarget.note hook body. It reports what the run did
// to the client-side allowlist — including, crucially, when it REMOVED one.
//
// A bare `plumb setup codex` resolves to leanClear and deletes the key. That is
// the only path in this feature that destroys user configuration, and it used to
// be the only silent one: the note short-circuited on "no --lean", so a user who
// followed `plumb doctor`'s own repoint advice went from 21 tools to 57 with
// nothing said. (The other half of that fix is repointFix, which keeps --lean on
// doctor's suggested command when an allowlist is in place.) The clear line is
// worded from the END STATE rather than from a diff, so it is true whether or not
// a key was actually there — the writer does not report which, and inventing a
// "removed" claim it cannot substantiate would be worse than describing what the
// entry now says.
//
// EVERY clause has to survive that test, not just the leading one. An earlier
// draft closed with "(the previous config was backed up alongside it)", which is
// false in two reachable states: a first-ever registration, where mergeServerEntry
// skips backupFile because the file is new, and an idempotent second bare run,
// which writes nothing at all and still printed the claim. It is deleted rather
// than hedged — three lines that are all true beat four with a caveat, and it
// also lightens the paragraph a first-time user sees about an allowlist they
// never had.
//
// leanKeep says nothing: that is a bulk sweep, or Kimi's bare re-register, and
// neither touches the key.
func leanSetupNote(c leanClient, choice leanChoice) string {
	switch choice {
	case leanPin:
		return fmt.Sprintf(
			"Tool allowlist: %s now pins the %d lean plumb tools client-side.\n"+
				"It is a snapshot, not a live view — re-run `plumb setup %s --lean` after a\n"+
				"plumb upgrade to refresh it. %s",
			c.key, len(tools.LeanToolNames()), c.setupCmd, c.bareNote)
	case leanClear:
		return fmt.Sprintf(
			"Tool allowlist: no --lean, so the plumb entry carries no client-side %s\n"+
				"allowlist and any that was there has been cleared — %s loads plumb's full\n"+
				"tool surface. Run `plumb setup %s --lean` to pin the %d lean tools instead.",
			c.key, c.name, c.setupCmd, len(tools.LeanToolNames()))
	}
	return ""
}

// kimiLeanChoice maps Kimi Code's boolean --lean flag to a choice. Kimi never
// clears: its shipped contract preserves the key on a bare re-register (see
// kimiCodeInto), so the flag is pin-or-leave-alone.
func kimiLeanChoice(lean bool) leanChoice {
	if lean {
		return leanPin
	}
	return leanKeep
}

// codexLeanInto registers plumb in Codex's TOML config, managing the per-server
// enabled_tools allowlist per choice. Codex merges rather than replaces, so a
// user's [mcp_servers.plumb.tools.*] approval tables survive both paths.
func codexLeanInto(cfgPath, plumbBin string, choice leanChoice) (added bool, preserved []string, err error) {
	return mergeLeanEntry(codexLeanClient, cfgPath,
		map[string]any{"command": plumbBin, "args": []string{"serve"}}, choice,
		func(existing map[string]any) bool {
			return existing["command"] == plumbBin && stringSliceEqual(existing["args"], []string{"serve"})
		},
	)
}

// geminiLeanInto registers plumb in Gemini CLI's settings.json, managing the
// per-server includeTools allowlist per choice.
//
// Gemini shares Claude Desktop's mcpServers shape and borrowed
// setupClaudeDesktopInto until --lean arrived. It needs its own writer now
// because the allowlist key is Gemini's alone: routing both through one function
// would put an includeTools key into a Claude Desktop config that does not read
// it.
func geminiLeanInto(cfgPath, plumbBin string, choice leanChoice) (added bool, preserved []string, err error) {
	return mergeLeanEntry(geminiLeanClient, cfgPath,
		map[string]any{"command": plumbBin, "args": []string{"serve"}}, choice,
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

// setupCodexInto and setupGeminiInto are the targets' intoFns: they resolve the
// CLI state (the --lean flag, and whether this is a bulk sweep) into a
// leanChoice and delegate. The split keeps the choice testable without touching
// package flag vars.
func setupCodexInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return codexLeanInto(cfgPath, plumbBin, leanChoiceOf(setupCodexLeanFlag))
}

func setupGeminiInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return geminiLeanInto(cfgPath, plumbBin, leanChoiceOf(setupGeminiLeanFlag))
}
