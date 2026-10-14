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

// configAuthorityEnabled is the gate for the CONFIG DIRECTORY alone, and it asks
// the SEED question rather than the resolved one.
//
// The file that turns the account authority off lives in the config directory,
// so a config directory that followed the RESOLVED value would move out from
// under its own setting: `plumb config set daemon.account_authority false` would
// write the account's file, and the next start — seeding from the environment,
// where nothing was set — would read the environment's. With the seed in charge
// the location is stable and the setting cannot hide itself. Every other global
// directory follows the resolved value; see internal/paths/authority.go.
func configAuthorityEnabled() bool {
	if testing.Testing() {
		return authorityFromEnv()
	}
	return seedAuthorityValue()
}
