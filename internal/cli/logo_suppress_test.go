package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// `plumb doctor --json` used to print the ASCII banner to stdout ahead of the
// JSON document, so every consumer failed to parse it on line 1. The logo-skip
// annotation could not fix that on its own: it is per-command, while
// machine-readable output is per-invocation of the same command.

func TestSuppressLogo_JSONFlagSet(t *testing.T) {
	cmd := &cobra.Command{Use: "doctor"}
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if !suppressLogo(cmd) {
		t.Error("--json must suppress the banner — it corrupts the JSON document on stdout")
	}
}

func TestSuppressLogo_JSONFlagUnset(t *testing.T) {
	cmd := &cobra.Command{Use: "doctor"}
	cmd.Flags().Bool("json", false, "")
	if suppressLogo(cmd) {
		t.Error("without --json the banner must still print")
	}
}

func TestSuppressLogo_AnnotationStillHonoured(t *testing.T) {
	cmd := &cobra.Command{Use: "serve", Annotations: map[string]string{annoSkipLogo: "true"}}
	if !suppressLogo(cmd) {
		t.Error("annoSkipLogo must keep suppressing the banner (MCP wire / alt-screen)")
	}
}

// A command with no json flag at all must not be affected.
func TestSuppressLogo_NoJSONFlag(t *testing.T) {
	if suppressLogo(&cobra.Command{Use: "sessions"}) {
		t.Error("a command without --json must print the banner")
	}
}

// The lifecycle-hook verbs are the same failure in a different costume: a hook
// runs unattended, its stdout goes to the client. For Codex that stdout IS a
// JSON document (a banner ahead of it fails the parse); for Claude Code's
// SessionStart, plain stdout is injected into the agent's context, where an
// ASCII banner is noise the model has to read past. Both must stay silent.
func TestSuppressLogo_HookRunVerbs(t *testing.T) {
	for _, cmd := range []*cobra.Command{hooksRunClaudeCmd, hooksRunCodexCmd} {
		if !suppressLogo(cmd) {
			t.Errorf("`plumb hooks %s` prints the banner — it reaches the client's stdout", cmd.Use)
		}
	}
}
