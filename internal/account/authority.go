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
// taste. The gate decides which CONFIG FILE plumb reads (paths.ConfigDir
// consults it), so it cannot be read from that file by the layer below: the
// directory a setting is written to would depend on the setting. internal/config
// — a higher layer, which may import this one — resolves the key and RECORDS the
// answer with SetResolvedAuthority; everything at or below foundation reads it.
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
// uses for the decisions that necessarily come first — above all which config
// file to read, and the daemon's runtime lifetime lock, which it takes before it
// reads any configuration at all.
func seedAuthorityValue() bool {
	if v, ok := ParseAuthorityEnv(); ok {
		return v
	}
	return defaultAuthority
}

// resolvedAuthorityValue is the gate this process resolved, or the seed when
// nothing has resolved one yet.
func resolvedAuthorityValue() bool {
	authorityMu.Lock()
	defer authorityMu.Unlock()
	if !authoritySet {
		authorityValue = seedAuthorityValue()
		authoritySet = true
	}
	return authorityValue
}

// SetResolvedAuthority records the value the configuration resolved for
// `daemon.account_authority`.
//
// The FIRST call wins. The gate decides where every global directory is — the
// runtime directory the daemon's sockets and locks live in, its data, state,
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
