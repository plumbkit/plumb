package cli

import (
	"github.com/spf13/cobra"
)

// Kimi Code was the first client to take --lean, so this file holds the parts of
// that contract that are Kimi's alone; the shared descriptor, choice machinery,
// and merge body live in setup_lean.go, which also records why a client-side
// allowlist exists at all and what plumb may claim about it.

// setupKimiLeanFlag backs `plumb setup kimi-code --lean`. It is a package-level
// flag var like setupClaudeCodeProjectFlag, read at registration time by the
// Kimi target's intoFn — the bulk --all / --install-missing paths never set it,
// so they register bare and preserve whatever allowlist is already on disk.
var setupKimiLeanFlag bool

// registerKimiLeanFlag is the setupTarget.flags hook for Kimi Code.
func registerKimiLeanFlag(cmd *cobra.Command) {
	leanFlagRegistrar(&setupKimiLeanFlag, kimiLeanClient)(cmd)
}

// kimiLeanNote is the setupTarget.note hook for Kimi Code.
func kimiLeanNote() string { return leanSetupNote(kimiLeanClient, setupKimiLeanFlag) }

// kimiCodeInto registers plumb in Kimi Code's mcp.json (the plain mcpServers
// JSON shape, shared with Kimi Desktop). With lean set it additionally writes
// the "enabledTools" allowlist — tools.LeanToolNames(), the same set plumb's own
// lean profile advertises.
//
// Kimi takes a BOOLEAN, not a leanChoice, because its shipped contract has no
// clearing path: a later bare re-register PRESERVES the allowlist
// (mergeServerEntry merges the canonical fields onto the existing entry, the
// same contract that keeps Codex's per-tool approval tables). Removing an
// allowlist is a one-line manual edit, and a flag that silently widens a surface
// the user narrowed on purpose is worse than none. Codex and Gemini, added
// later, take the symmetric contract instead (leanChoiceOf) so that the flag
// state on the command line always matches what lands in the file.
func kimiCodeInto(cfgPath, plumbBin string, lean bool) (added bool, preserved []string, err error) {
	choice := leanKeep
	if lean {
		choice = leanPin
	}
	return mergeLeanEntry(kimiLeanClient, cfgPath,
		map[string]any{"command": plumbBin, "args": []string{"serve"}}, choice,
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}
