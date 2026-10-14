package config

// config_daemon.go holds the [daemon] section: the account-wide service itself,
// and the one switch that decides where it lives. Split out of config.go, which
// is at the 600-line cap, the same way [collab] and [git] are.

// DaemonConfig is the [daemon] section: the account-wide service itself.
//
// Concurrency: read-only after Load returns.
type DaemonConfig struct {
	// AccountAuthority resolves plumb's global directories — runtime, data,
	// state, logs, caches — and what a managed launch starts, from the OS
	// ACCOUNT (and, once `plumb service install` has run, from the service
	// profile) instead of from this process's environment. With it on, an
	// upgraded `plumb serve` starts the profile's executable with a clean
	// environment, so no client's HOME, XDG or PATH can retarget the account's
	// one daemon.
	//
	// Default TRUE: from this release every shipped build resolves the account's
	// one daemon, its sockets and its state, and launches the installed binary.
	//
	// FALSE IS AN OPT-OUT, NOT A DISARMING. It returns the runtime, data, state,
	// log and cache directories — and the launch — to the environment-derived
	// resolution they had before this setting existed. It does NOT stop plumb
	// taking the account authority lock, and it does NOT stop a launch refusing
	// to start a daemon beside one that holds it, so an opted-out plumb can never
	// run two daemons: the property PLAN-518 exists for is not configurable. It
	// does not move the CONFIG file either — the directory that declares this
	// setting must not depend on it, or a config write would land in a file the
	// next start never reads (internal/paths/authority.go's configDir, and
	// docs/configuration.md for the user-facing statement).
	//
	// Env: PLUMB_ACCOUNT_AUTHORITY — any value other than 0/false/no turns it on,
	// and every managed launch passes the value it resolved down to the daemon it
	// starts, so the two cannot disagree about where the daemon lives.
	// Restart-needed: the gate decides where a running daemon's sockets and
	// state live, so it is read once, at config load, and a later change is
	// reported by `plumb config show` and the daemon as needing a restart.
	AccountAuthority bool `toml:"account_authority"`
}
