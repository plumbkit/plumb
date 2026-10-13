// Package account is the single source of truth for WHO this plumb process is
// running as, and therefore for where the account-wide files live (PLAN-527).
//
// It exists because every other answer used to come from the environment.
// `plumbRuntimeDir()` derived the daemon's singleton from $HOME, so every HOME
// override was a legal second daemon — which is how 64 of them leaked — and a
// client's environment could retarget the daemon's config, caches and logs.
// This package never consults $HOME, $USER or os.UserHomeDir: it asks the OS
// account database, and when that cannot answer it refuses rather than guessing.
// A wrong answer here is worse than no answer, because it would silently move
// the daemon somewhere else and start a second one.
//
// Pure-Go Linux is the trap: os/user's lookupStubs "looks up" the current user
// from $USER and $HOME when the passwd lookup is unavailable, which is exactly
// the fallback this package exists to refuse. On a CGO_ENABLED=0 linux build the
// passwd file is parsed directly instead, and a uid that is not in it is a
// refusal.
//
// Concurrency: safe for concurrent use; every value is resolved once per call
// from immutable inputs.
package account

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// User is the account this process runs as.
type User struct {
	// UID is the real user id, as the OS reports it.
	UID int
	// Home is the account's home directory, from the account database.
	Home string
}

// ErrNoAccount reports that the account database could not be consulted or does
// not know this uid. Callers must treat it as fatal: there is no fallback, by
// design.
var ErrNoAccount = errors.New("cannot determine this account from the OS account database")

// Current resolves the account this process runs as, or fails closed.
func Current() (User, error) {
	if root := testRoot(); root != "" {
		return User{UID: os.Getuid(), Home: root}, nil
	}
	if err := testBinaryRefusal(); err != nil {
		return User{}, err
	}
	return lookup(os.Getuid())
}

// lookup asks the platform's account database for uid.
func lookup(uid int) (User, error) {
	home, ok := platformHome(uid)
	if !ok {
		return User{}, fmt.Errorf("%w: uid %d", ErrNoAccount, uid)
	}
	if !filepath.IsAbs(home) || home == "/" {
		// A relative or root home is not a place to keep a daemon's lock file.
		return User{}, fmt.Errorf("%w: uid %d has no usable home directory (%q)", ErrNoAccount, uid, home)
	}
	return User{UID: uid, Home: home}, nil
}

// DataDir returns the account's private data directory for plumb: the home for
// things that must outlive the runtime directory and must not be swappable by an
// environment variable. It is where the authority lock and, from PLAN-528, the
// service profile live.
//
//   - darwin:  <home>/Library/Application Support/plumb
//   - linux:   <home>/.local/share/plumb
//
// It is deliberately NOT the XDG data directory: this is the account-wide
// service's own anchor, and honouring $XDG_DATA_HOME here would hand the
// retargeting problem straight back.
func (u User) DataDir() string {
	return filepath.Join(u.Home, dataDirRelative())
}

func dataDirRelative() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join("Library", "Application Support", "plumb")
	}
	return filepath.Join(".local", "share", "plumb")
}

// CacheDir returns the account's cache directory for plumb — the canonical home
// of the daemon's runtime files (sockets, pid, version, lifetime lock).
//
//   - darwin: <home>/Library/Caches/plumb  (os.UserCacheDir ignores
//     $XDG_CACHE_HOME there, so the environment cannot move it anyway)
//   - linux:  <home>/.cache/plumb
//
// On Linux this is the FALLBACK location: the canonical runtime directory is
// /run/user/<uid>/plumb when that is usable, see RuntimeDir.
func (u User) CacheDir() string {
	return filepath.Join(u.Home, cacheDirRelative())
}

func cacheDirRelative() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join("Library", "Caches", "plumb")
	}
	return filepath.Join(".cache", "plumb")
}

// ConfigDir returns the account's config directory for plumb.
//
//   - darwin:  <home>/Library/Application Support/plumb
//   - linux:   <home>/.config/plumb
//
// The Linux spelling is deliberately the XDG default rather than
// $XDG_CONFIG_HOME: this is the account-wide service's own anchor, and honouring
// a per-process variable here is the retargeting problem in one line. An
// explicit XDG setup is adopted into the service profile at install (PLAN-528).
func (u User) ConfigDir() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.Home, "Library", "Application Support", "plumb")
	}
	return filepath.Join(u.Home, ".config", "plumb")
}

// StateDir returns the account's state directory for plumb (regenerable state:
// session state, caches of derived facts).
//
//   - darwin:  <home>/Library/Application Support/plumb
//   - linux:   <home>/.local/state/plumb
func (u User) StateDir() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.Home, "Library", "Application Support", "plumb")
	}
	return filepath.Join(u.Home, ".local", "state", "plumb")
}

// LogDir returns the account's log directory for plumb. macOS keeps user logs in
// ~/Library/Logs, which is the directory Console.app reads; everywhere else logs
// belong with state.
func (u User) LogDir() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.Home, "Library", "Logs", "plumb")
	}
	return u.StateDir()
}

// appDir is plumb's own directory name under a runtime base.
const appDir = "plumb"

// seamRuntimeDirRelative is where the runtime directory sits under a seam root:
// a private "run" base with plumb under it, the shape a real runtime base gives
// the daemon (and the shape a HOME-derived $XDG_RUNTIME_DIR takes). It is
// deliberately NOT the cache directory, RuntimeDir's other answer, so a test can
// tell which of the two it got.
const seamRuntimeDirRelative = "run/plumb"

// RuntimeDir returns the canonical runtime directory for this account: where the
// daemon's sockets, pid file, version file and lifetime lock live, and therefore
// the account-wide singleton's address.
//
// A seam root outranks every other answer, on every platform: the runtime
// directory is an account path like the rest, and the seam exists so that a test
// binary cannot touch the real one. Without that, a Linux test run — where
// /run/user/<uid> is a real, usable directory — resolved /run/user/<uid>/plumb,
// and a test's daemon could bind its socket in the real account runtime
// directory, which is exactly the cross-contamination the seam prevents.
//
// Otherwise, on Linux, it prefers /run/user/<uid>/plumb when the runtime
// directory the kernel provides is present, a directory, owned by this uid and
// mode 0700 — the spec's own checks, not a trusted variable. Elsewhere (and
// always on darwin) it is the cache directory above.
func (u User) RuntimeDir() string {
	if root := testRoot(); root != "" {
		return filepath.Join(root, seamRuntimeDirRelative)
	}
	if dir, ok := linuxRuntimeDir(u.UID); ok {
		return dir
	}
	return u.CacheDir()
}

// AuthorityLockPath returns the file whose exclusive flock is the account's
// launch authority: held by the one daemon for its whole life, taken by every
// managed launch before it starts a child (PLAN-527, generalising #649).
//
// It sits in the DATA directory rather than the runtime directory on purpose:
// the runtime directory is removable (a tmpfs, or a directory somebody deletes),
// and the authority must survive that — a lock file that vanished with its
// directory would let a second daemon take "the" authority from scratch.
func (u User) AuthorityLockPath() string {
	return filepath.Join(u.DataDir(), "daemon.authority.lock")
}

// ServiceProfilePath returns the path of the service-owned launch profile
// (PLAN-528): what `plumb serve` launches, and the global directories that build
// inherits.
func (u User) ServiceProfilePath() string {
	return filepath.Join(u.DataDir(), "service.toml")
}

// AuthorityEnabled reports whether the account authority drives this process:
// its global directories come from the OS account database, and its daemon
// launches take the account authority lock before starting a child.
//
// Until the default flip it is reachable only in a test or a container harness
// — see the seam files for the two independent conditions that guard it. The
// user-facing switch is a config key that lands with PLAN-530's legacy detection
// and the default flip (PLAN-518 rollout invariant 1), because a build in which
// an upgraded serve can start a second daemon beside an old-path one must never
// reach a user.
func AuthorityEnabled() bool { return authorityEnabled() }

// envTrue reports whether name is set to one of the values a boolean switch
// accepts. Anything else — including unset — is false, which is the safe
// reading for a gate: an unrecognised value must not turn it on.
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// testRoot reports the directory a test has redirected account resolution to,
// or "" when none applies. It is honoured ONLY where the seam exists — see
// seam.go and seam_off.go — so no production build can take this path.
func testRoot() string { return seamRoot() }

// testBinaryRefusal refuses account resolution in a test binary that has not
// declared a seam root.
//
// This is the difference between a test suite that isolates and a test suite
// that quietly uses the developer's real account: with the authority lock under
// the real account's data directory, a unit test that took it would make the
// owner's own `plumb serve` wait out its whole start deadline and then fail. The
// precedent is internal/session/dir.go, which refuses the live session registry
// the same way.
func testBinaryRefusal() error {
	if !testing.Testing() {
		return nil
	}
	return fmt.Errorf("%w: refusing in a test binary without PLUMB_TEST_ACCOUNT_ROOT — "+
		"a test that resolved the real account's directories could take the running daemon's authority lock", ErrNoAccount)
}

// linuxRuntimeDir returns /run/user/<uid>/plumb when the kernel's per-user
// runtime directory is usable.
func linuxRuntimeDir(uid int) (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	return runtimeDirUnder("/run/user/"+strconv.Itoa(uid), uid)
}

// runtimeDirUnder applies the checks the XDG base directory spec requires of a
// consumer before it trusts a runtime base — the base must already exist, be a
// directory, be owned by this uid and be mode 0700, because the daemon socket
// would otherwise be exposed to whoever owns the directory — and names plumb's
// directory under it.
//
// The base is a parameter rather than the constant /run/user/<uid> so these
// checks can be exercised on any platform: they otherwise run only on Linux,
// which is how a darwin-only pass let the runtime directory escape the seam
// root.
func runtimeDirUnder(base string, uid int) (string, bool) {
	info, err := os.Stat(base) //nolint:gosec // G304: the base is this process's own uid's runtime directory, or a test's own temp dir
	if err != nil || !info.IsDir() {
		return "", false
	}
	if info.Mode().Perm() != 0o700 {
		return "", false
	}
	if !ownedByUID(info, uid) {
		return "", false
	}
	return filepath.Join(base, appDir), true
}

// ParseTestRoot validates a seam root from the environment. Exported for the
// packages that read it, and testable on its own.
func ParseTestRoot(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !filepath.IsAbs(v) {
		return ""
	}
	return v
}
