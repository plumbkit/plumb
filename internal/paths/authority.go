package paths

import (
	"github.com/plumbkit/plumb/internal/account"
)

// The account-authority mode (PLAN-527).
//
// Plumb used to resolve every global directory from the environment, which is
// why a client with a different HOME was a legal second daemon. With the
// authority enabled, the directories come from the OS account database instead:
// the same answer whatever the calling process's environment says, and the
// anchor the launch authority and the service profile hang off.
//
// THE GATE IS NOT USER-SETTABLE YET. The account authority only reaches users as
// one transition (PLAN-518 rollout invariant 1): until the legacy detection of
// PLAN-530 is in, no release may contain a build where an upgraded serve can
// start a second daemon beside an old-path one. So pre-flip,
// account.AuthorityEnabled is true only for a test or a container harness, and
// the user-facing switch lands with the default flip.

// AuthorityEnabled reports whether this process resolves its global directories
// — and launches its daemon — through the account authority.
func AuthorityEnabled() bool { return account.AuthorityEnabled() }

// accountDir returns the account-derived directory for this process when the
// authority is enabled, and reports whether it applied.
//
// A failure to resolve the account reports false, which leaves the caller on the
// legacy path. That is not a silent fallback in a running process: the entry
// points that matter — the daemon and every launcher — resolve the account first
// and refuse to start without it (see internal/cli's accountAuthorityEnabled
// call sites), so a process that got this far either resolved the account or is
// on the legacy path by design.
func accountDir(pick func(account.User) string) (string, bool) {
	if !account.AuthorityEnabled() {
		return "", false
	}
	u, err := account.Current()
	if err != nil {
		return "", false
	}
	return pick(u), true
}
