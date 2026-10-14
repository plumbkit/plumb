package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// The [daemon] section of `plumb config show`: the row exists for the same
// reason it exists in the TUI — "which mode is my daemon in?" is the first
// question an operator asks when the socket is somewhere unexpected — and the
// row is only useful if it can name the layer that decided.

func TestDaemonConfigRows_NameTheirEnvVars(t *testing.T) {
	if got := envVarForField("account_authority"); got != "PLUMB_ACCOUNT_AUTHORITY" {
		t.Fatalf("envVarForField(%q) = %q, want PLUMB_ACCOUNT_AUTHORITY — `plumb config show` could not name "+
			"the variable that overrode the account authority", "account_authority", got)
	}
}

func TestDaemonConfigRow_EnvVarWins(t *testing.T) {
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "false")

	// The row is built from the merged view; a project cannot move this value, so
	// base and merged agree and the ENV layer is the one that has to show.
	global := config.Defaults()
	merged := config.Defaults()
	merged.Daemon.AccountAuthority = false

	got := sourceFor("account_authority",
		config.Defaults().Daemon.AccountAuthority, global.Daemon.AccountAuthority, merged.Daemon.AccountAuthority)
	if !strings.Contains(got, "PLUMB_ACCOUNT_AUTHORITY") {
		t.Fatalf("account_authority's source column is %q; it must name PLUMB_ACCOUNT_AUTHORITY when that "+
			"variable is what won", got)
	}
}

// The reload table is the user-facing half of RestartSensitiveEqual: a setting
// the daemon cannot apply live must be listed as needing a restart, or the user
// changes it, sees no effect, and concludes the setting is broken.
func TestReloadBehaviourListsTheAccountAuthority(t *testing.T) {
	// The table is printed rather than returned, so this pins the property it is
	// built from: the tier the registry declares, which is what the table's
	// restart row is supposed to mirror.
	f, ok := config.Lookup("daemon.account_authority")
	if !ok {
		t.Fatal("daemon.account_authority is not in the field registry, so the reload table cannot classify it")
	}
	if f.ReloadTier != config.ReloadRestart {
		t.Errorf("daemon.account_authority is tier %v, want restart: it decides where the daemon's sockets and state live", f.ReloadTier)
	}
}
