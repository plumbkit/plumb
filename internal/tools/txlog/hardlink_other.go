//go:build !unix

package txlog

import "errors"

// refuseMultiplyLinked has no portable implementation off unix: the link count
// is not exposed by os.FileInfo.
//
// It FAILS CLOSED. plumb does not currently build for Windows (internal/session
// has no flock implementation there), so this file exists to keep the package
// compiling under a cross-build rather than to serve a supported platform —
// and a security check that cannot run must refuse, not wave the file through.
func refuseMultiplyLinked(string) error {
	return errors.New("hard-link detection is unavailable on this platform, so an " +
		"untrusted snapshot cannot be verified")
}
