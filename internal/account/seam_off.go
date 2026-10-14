//go:build !plumbtest

package account

import (
	"os"
	"testing"
)

// The seam WITHOUT the plumbtest tag: available only inside a test binary.
//
// testing.Testing() is a property of the binary, not of the environment: it is
// true when the process was built by `go test` and false in every shipped
// binary, so a release build has no way to reach this function's return value.
// That is the whole safety argument, and it is why the real seam (seam.go) adds
// a build tag on top: a CHILD process is not a test binary, so the smoke suites
// need the tag to give their children the same isolation.
func seamRoot() string {
	if !testing.Testing() {
		return ""
	}
	return ParseTestRoot(os.Getenv("PLUMB_TEST_ACCOUNT_ROOT"))
}

// authorityEnabled reports whether the account authority drives this process.
//
// In a test binary it is the seam's own switch and nothing else, so the suite
// keeps the legacy resolution it has always had unless a test asks for the
// canonical one. The default flip may not promote a test into canonical mode:
// that would have every existing test resolve (and contend for) the developer's
// real account directories.
//
// Everywhere else — a release build, whose binaries are the only ones users
// run — it is the resolved configuration. The default is now ON: PLAN-530's
// legacy detection landed first, so an upgraded serve that takes the authority
// can no longer start beside an old-path daemon, and `daemon.account_authority`
// is how a user turns it back off.
func authorityEnabled() bool {
	if testing.Testing() {
		return authorityFromEnv()
	}
	return resolvedAuthorityValue()
}

// configAuthorityEnabled reports whether plumb's CONFIG directory may be the
// account's: the file that declares `daemon.account_authority` must not move
// when the setting changes, or the setting hides itself from the next start
// (`plumb config set` would write a file nothing reads).
//
// So the config directory follows NEITHER the seed nor the resolved value in a
// released build — it is the account's, always. An environment opt-out
// (PLUMB_ACCOUNT_AUTHORITY=false) moves the runtime directory, the sockets, the
// data, the state and the logs, and deliberately NOT the config file: a
// directory that moved with the switch would leave the switch unreadable, and
// the two opt-out routes would then disagree about which file plumb reads. For
// the same reason the location is not a function of the resolved value either,
// which would move the file on the start AFTER the one that read it.
//
// In a test binary the seam decides, exactly as authorityEnabled does, so a test
// still isolates its config through HOME/XDG and never touches the developer's
// real account directory. internal/paths/authority.go's configDir is the only
// reader; docs/configuration.md states the behaviour for users.
func configAuthorityEnabled() bool {
	if testing.Testing() {
		return authorityFromEnv()
	}
	return true
}
