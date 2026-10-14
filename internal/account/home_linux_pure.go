//go:build linux && !cgo

package account

import (
	"context"
	"os"
)

// platformHome reads uid's home directory from the OS account database: the
// passwd file first, then getent.
//
// This build cannot ask os/user: without cgo, its linux implementation is a stub
// that answers from $USER and $HOME (go/src/os/user/lookup_stubs.go), so a
// missing passwd entry would come back as a confident answer built from the
// environment — the one thing this package must never accept.
//
// /etc/passwd alone is not enough, because this is the build every Linux release
// binary ships as: a host whose accounts come from NSS (LDAP, SSSD, Active
// Directory, systemd-homed, NIS) has no entry for them there, and authorityPath
// is consulted unconditionally on the way to starting a daemon, so refusing on
// the file alone is a host where no daemon can ever start. getent asks the same
// name service the C library does and reads no environment variable, so the
// fail-closed property is unchanged: only BOTH sources failing is a refusal.
//
// The getent half — runGetent and the exec it drives — is in getent_linux.go,
// which every Linux build compiles rather than only this one, so a test can run
// the real binary on a host where it is not the production path.
func platformHome(uid int) (string, bool) {
	// A read that fails is not an answer: the file is then treated as one holding
	// no entry for uid, so the NSS source can still speak for it.
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		data = nil
	}
	return lookupHome(context.Background(), string(data), uid, runGetent)
}
