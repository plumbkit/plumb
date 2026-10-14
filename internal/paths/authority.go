package paths

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/service"
)

// The account-authority mode (PLAN-527) and the service profile (PLAN-528).
//
// Plumb used to resolve every global directory from the environment, which is
// why a client with a different HOME was a legal second daemon and why a branch
// build could cold-start the account's daemon rooted in temporary directories.
// With the authority enabled the directories come from the OS account database,
// and the SERVICE PROFILE (PLAN-528) records them once, at install, so that what
// a client's environment says can never move them again.
//
// THE GATE IS USER-SETTABLE NOW. It is `daemon.account_authority`, default TRUE,
// resolved by internal/config and recorded in internal/account (PLAN-518 D3) —
// the default flip, which PLAN-530's legacy detection made safe to ship. In
// LEGACY mode the profile is ignored entirely — not read, not consulted, not
// missed — and every directory comes from the environment again.
//
// ONE DIRECTORY DELIBERATELY DOES NOT FOLLOW IT. Read configDir below: the
// config directory is the account's in every released build, whatever the switch
// says, because the file that declares the switch must not move with it — a
// `daemon.account_authority` living in a directory that depended on its own
// value could hide itself from the next start.
//
// What the gate does NOT decide is whether the authority is taken: every managed
// launch takes the account authority lock and refuses to start beside a holder
// in every mode, opted out included (PLAN-518 D4).

// AuthorityEnabled reports whether this process resolves its global directories
// — and launches its daemon — through the account authority.
func AuthorityEnabled() bool { return account.AuthorityEnabled() }

// accountDir returns the directory this process uses for role when the authority
// is enabled, and reports whether it applied.
//
// The profile comes first and the account standard path second: a profile that
// records a directory is the service's own statement of where it serves from,
// and the account path is the profile's documented default for a role it does
// not override. An ABSENT profile leaves the account path in place — that is the
// default, not an inference — while a launch, which needs an executable and a
// PATH rather than a directory, refuses outright (see internal/cli).
//
// A failure to resolve the account reports false, which leaves the caller on the
// legacy path. That is not a silent fallback in a running process: the entry
// points that matter — the daemon and every launcher — resolve the account first
// and refuse to start without it, so a process that got this far either resolved
// the account or is on the legacy path by design.
func accountDir(role service.Role) (string, bool) {
	if !account.AuthorityEnabled() {
		return "", false
	}
	u, err := account.Current()
	if err != nil {
		return "", false
	}
	if dir, ok := profileDir(role); ok {
		return dir, true
	}
	return accountDefaultDir(u, role), true
}

// configDir is accountDir for the CONFIG role, and it asks a different question
// on purpose: whether the ACCOUNT's config directory may be the one this process
// reads. In a released build the answer is always yes (see
// account.ConfigAuthorityEnabled); only a test binary or a container harness,
// which says which directories it wants and must never reach the developer's
// real account directory, resolves the config directory from the environment.
//
// WHY THE CONFIG DIRECTORY IS NOT PART OF THE SWITCH. The file that declares
// `daemon.account_authority` is the file this resolves, so a directory that moved
// with the setting would move out from under its own declaration. Set
// `daemon.account_authority = false` and a resolved-value rule would send the
// NEXT start to the environment's file — a different file wherever
// XDG_CONFIG_HOME differs from the account standard — so the opt-out would be
// read exactly once, and the next write of the global config file (the TUI, or an
// editor) would land in a file nothing ever read. An ENV opt-out rule has the same
// defect from the other side: that write under PLUMB_ACCOUNT_AUTHORITY=false would
// land in the environment's file, and the next start, seeded from an environment
// without the variable, would read the account's. The config file therefore
// follows neither the seed nor the resolved value: it stays the account's, both
// routes stay readable, and the switch keeps working in both directions.
//
// Every other global directory follows the resolved value, and
// docs/configuration.md states both halves for users.
func configDir() (string, bool) {
	if !account.ConfigAuthorityEnabled() {
		return "", false
	}
	u, err := account.Current()
	if err != nil {
		return "", false
	}
	if dir, ok := profileDir(service.RoleConfig); ok {
		return dir, true
	}
	return u.ConfigDir(), true
}

// profileDir returns the directory the service profile records for role.
//
// An unreadable or malformed profile is reported ONCE per process and then
// falls back to the account standard paths, rather than failing every directory
// lookup in the process: a profile that cannot be parsed is a broken install,
// and the remedy (`plumb service install`) is named in the warning. A launch
// refuses on the same error instead of warning, because launching the wrong
// binary is unrecoverable where serving from the default directory is not.
func profileDir(role service.Role) (string, bool) {
	p, err := service.Load()
	if err != nil {
		if !isMissing(err) {
			warnProfileOnce(err)
		}
		return "", false
	}
	dir := p.Dirs.Dir(role)
	return dir, dir != ""
}

// isMissing reports whether err is the absence of a profile, which is not a
// fault to warn about: a legacy install simply has none.
func isMissing(err error) bool {
	return errors.Is(err, service.ErrNoProfile)
}

// warnProfileOnce reports a profile that exists but cannot be used, once per
// process. Directory resolution runs on nearly every command, and a warning per
// call would bury the message it is trying to deliver.
func warnProfileOnce(err error) {
	profileWarn.Do(func() {
		slog.Warn("plumb: the service profile is unreadable; using the account standard directories",
			"err", err, "remedy", "plumb service install")
	})
}

var profileWarn sync.Once

// accountDefaultDir is the account standard directory for role: the value the
// profile records when nothing has been adopted for it.
func accountDefaultDir(u account.User, role service.Role) string {
	switch role {
	case service.RoleConfig:
		return u.ConfigDir()
	case service.RoleData:
		return u.DataDir()
	case service.RoleState:
		return u.StateDir()
	case service.RoleLog:
		return u.LogDir()
	case service.RoleCache:
		return u.CacheDir()
	default:
		return ""
	}
}

// EnvVarForRole names the XDG variable that governs role, or "" when none does.
// It is what `plumb service install` and `plumb service set-xdg` adopt by, and
// what the divergence report tells a user to change.
func EnvVarForRole(role service.Role) string {
	switch role {
	case service.RoleConfig:
		return "XDG_CONFIG_HOME"
	case service.RoleData:
		return "XDG_DATA_HOME"
	case service.RoleState:
		return "XDG_STATE_HOME"
	case service.RoleLog:
		if runtime.GOOS == "darwin" {
			// macOS keeps user logs in ~/Library/Logs, which no XDG variable
			// governs, so there is nothing to adopt.
			return ""
		}
		return "XDG_STATE_HOME"
	case service.RoleCache:
		return "XDG_CACHE_HOME"
	default:
		return ""
	}
}

// RoleEnvOverride returns the XDG override this process's environment sets for
// role, when it sets an absolute one. It is how the installer decides what to
// ADOPT: a real XDG setup is recorded explicitly rather than silently dropped.
func RoleEnvOverride(role service.Role) (name, value string, ok bool) {
	name = EnvVarForRole(role)
	if name == "" {
		return "", "", false
	}
	v := os.Getenv(name)
	if v == "" || !filepath.IsAbs(v) {
		return "", "", false
	}
	return name, v, true
}

// Divergence records a global directory this process's ENVIRONMENT would have
// resolved somewhere other than where the account service keeps it.
type Divergence struct {
	Role    string
	Env     string
	Service string
}

// ClientEnvDivergence returns the roles whose environment-derived answer differs
// from the one in use, so a caller can say once, per client, that a HOME or XDG
// override was ignored. It is empty in legacy mode, where the environment IS
// the answer, and it never fails: an unresolvable account simply reports nothing.
func ClientEnvDivergence() []Divergence {
	if !account.AuthorityEnabled() {
		return nil
	}
	var out []Divergence
	for _, role := range service.Roles() {
		svc, ok := accountDir(role)
		if !ok {
			continue
		}
		if env := envDir(role); env != "" && env != svc {
			out = append(out, Divergence{Role: string(role), Env: env, Service: svc})
		}
	}
	if svc, ok := accountRuntimeDir(); ok {
		if env := filepath.Join(runtimeBase(), appDir); env != svc {
			out = append(out, Divergence{Role: "runtime", Env: env, Service: svc})
		}
	}
	return out
}

// accountRuntimeDir returns the runtime directory the account service uses: the
// profile's adopted XDG_RUNTIME_DIR when one is recorded, else the account's own
// answer (a checked /run/user/<uid>, or the cache directory).
//
// The runtime directory is the one global location that is not a [dirs] entry:
// it is derived, and its override is the XDG variable that names it, so a client
// and the daemon it talks to resolve the same socket.
//
// The recorded base is re-checked here, on every read, against the XDG spec's
// own terms (a directory this uid owns, mode 0700) — the check both writers
// already make (install's adoption and set-xdg), because the daemon binds its
// MCP and control sockets under it and a hand-edited profile must not be trusted
// because one writer was correct (PLAN-550). A base that fails is REFUSED, never
// used: the answer is the account's own runtime directory, which every reader of
// the same file reaches the same way, so no two clients disagree about the socket
// path; and the warning names the remedy. The rest of the profile still holds —
// a bad runtime base must not move the config or data directories too.
func accountRuntimeDir() (string, bool) {
	if !account.AuthorityEnabled() {
		return "", false
	}
	u, err := account.Current()
	if err != nil {
		return "", false
	}
	if p, err := service.Load(); err == nil && p.XDG.RuntimeDir != "" {
		if account.UsableRuntimeBase(p.XDG.RuntimeDir, u.UID) {
			return filepath.Join(p.XDG.RuntimeDir, appDir), true
		}
		runtimeBaseWarn.Do(func() {
			slog.Warn("plumb: the service profile's XDG_RUNTIME_DIR is not a usable runtime directory; "+
				"using the account's own runtime directory",
				"recorded", p.XDG.RuntimeDir, "using", u.RuntimeDir(),
				"remedy", "make it a 0700 directory you own, or drop it: plumb service set-xdg --runtime-dir=")
		})
	}
	return u.RuntimeDir(), true
}

var runtimeBaseWarn sync.Once
