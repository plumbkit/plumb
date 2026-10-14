package account

import (
	"os"
	"sync"
)

// The resolved gate (PLAN-518 Phase 0A, the default flip).
//
// AuthorityEnabled used to be reachable only from a test or a container
// harness: no shipped build could take it, because rollout invariant 1 forbids a
// release in which an upgraded serve can start a second daemon beside an
// old-path one. PLAN-530 landed the legacy detection that closes that window, so
// the gate is now a real, user-facing setting — `daemon.account_authority`,
// default true — and this file is where its VALUE lives.
//
// It lives here rather than in internal/config, and that is not an accident of
// taste. The directories it governs are resolved by internal/paths, which is
// foundation and may not import internal/config — and the config file itself is
// read through that same resolver, so the key cannot be read from the file by
// the layer that decides which file it is. internal/config — a higher layer,
// which may import this one — resolves the key and RECORDS the answer with
// SetResolvedAuthority; everything at or below foundation reads it.
//
// Two values, not one, and the difference is load-bearing:
//
//   - the SEED is the environment plus the compiled default. It is what a
//     process knows before it has read anything, and it is what the launcher
//     passes to the daemon it starts, so both resolve the same directories.
//   - the RESOLVED value is what the configuration said, and it is what every
//     other global directory follows.
//
// Concurrency: safe for concurrent use. The value is decided once, early, by the
// first configuration resolution in the process, and only read afterwards.
// Nothing here takes a lock on a file or touches the filesystem.

// AuthorityEnvVar is the environment override for the account authority.
const AuthorityEnvVar = "PLUMB_ACCOUNT_AUTHORITY"

// ParseAuthorityEnv reads AuthorityEnvVar. ok is false when it is unset or
// empty, and the caller's own default then applies.
//
// Any value other than "0", "false" or "no" turns the authority ON. That is the
// reading internal/config gives every other default-true key (envBoolNeg:
// PLUMB_FSYNC, PLUMB_BLOCK_DIRTY_WRITES, PLUMB_GIT_ALLOW_WRITES), and it is the
// safe one here for a different reason: the default is already on, so a value
// nobody anticipated must not be the thing that quietly moves the account's
// daemon back into $HOME.
//
// internal/config's applyEnv calls this for the SAME key rather than parsing the
// string a second time. Two parsers for one variable is how a gate and its own
// configuration come to disagree about whether a value means on.
func ParseAuthorityEnv() (v bool, ok bool) {
	s := os.Getenv(AuthorityEnvVar)
	if s == "" {
		return false, false
	}
	return s != "0" && s != "false" && s != "no", true
}

// defaultAuthority is the compiled default: the account authority is ON.
const defaultAuthority = true

// authorityFromEnv is the reading a seam-governed process gets: an explicit
// value wins, and the default is OFF.
//
// It is deliberately not defaultAuthority. Every test binary and every
// container-harness child expects the legacy, environment-derived resolution
// unless it asked for the canonical one, so that the flip turns on code that is
// already tested rather than promoting the existing suite into writing to a
// developer's real account directories (PLAN-518 D5).
func authorityFromEnv() bool {
	v, ok := ParseAuthorityEnv()
	return ok && v
}

var (
	authorityMu    sync.Mutex
	authorityValue bool
	authoritySet   bool
)

// seedAuthorityValue is the gate as the ENVIRONMENT and the compiled default
// state it, before any configuration file has been read. It is what a process
// uses for the decisions that necessarily come first — above all the daemon's
// runtime lifetime lock, which it takes before it reads any configuration at
// all. Every managed launch passes its own resolved value down in
// PLUMB_ACCOUNT_AUTHORITY (internal/cli's daemonCommand), so a daemon's seed IS
// the mode its launcher resolved and the two agree on where the daemon lives.
func seedAuthorityValue() bool {
	if v, ok := ParseAuthorityEnv(); ok {
		return v
	}
	return defaultAuthority
}

// resolvedAuthorityValue is the gate this process resolved, or the seed when
// nothing has resolved one yet.
//
// A READ BEFORE THE FIRST RESOLUTION DOES NOT DECIDE. The seed is returned and
// the process is left unresolved, so the configuration loaded later still
// answers — which matters because the daemon resolves the runtime directory its
// lifetime lock lives in BEFORE it reads any configuration (it takes its locks
// first, by design), and latching the seed there would silently ignore a
// `daemon.account_authority = false` the user had written down. A process that
// reads the gate before it can know it uses the compiled answer until it does
// know it.
func resolvedAuthorityValue() bool {
	authorityMu.Lock()
	defer authorityMu.Unlock()
	if !authoritySet {
		return seedAuthorityValue()
	}
	return authorityValue
}

// AuthorityResolved reports the value the CONFIGURATION resolved for
// `daemon.account_authority`, and the compiled default before anything has
// resolved it. It is the gate every global directory except the config
// directory follows.
//
// It is not AuthorityEnabled, and the difference is the seam: AuthorityEnabled
// is the gate THIS process uses — a test binary or a container harness reads it
// from the environment, so the suite keeps the environment-derived resolution it
// was written against — while this reports what the configuration decided. The
// two agree in every released build. internal/config's tests use this to pin the
// wiring that publishes the key, and `runDaemon` logs it.
func AuthorityResolved() bool { return resolvedAuthorityValue() }

// SetResolvedAuthority records the value the configuration resolved for
// `daemon.account_authority`.
//
// The FIRST resolution wins. The gate decides where every global directory is —
// the runtime directory the daemon's sockets and locks live in, its data, state,
// logs and caches — and it is tier ReloadRestart precisely because that cannot
// change under a process that is already running. A later reload therefore
// cannot move them, and `plumb config show`'s restart-needed report is what
// tells the user to restart instead.
//
// internal/config.Load calls this; nothing else may.
func SetResolvedAuthority(v bool) {
	authorityMu.Lock()
	defer authorityMu.Unlock()
	if authoritySet {
		return
	}
	authorityValue, authoritySet = v, true
}

// resetAuthorityForTest clears the recorded value so a test can drive the seed
// and the resolution independently. Unexported, and reachable only from this
// package's tests: no production path may un-decide this.
func resetAuthorityForTest() {
	authorityMu.Lock()
	authorityValue, authoritySet = false, false
	authorityMu.Unlock()
}

// authorityRecordedForTest reports whether anything has resolved the gate yet.
func authorityRecordedForTest() bool {
	authorityMu.Lock()
	defer authorityMu.Unlock()
	return authoritySet
}
