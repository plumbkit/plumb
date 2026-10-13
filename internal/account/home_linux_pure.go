//go:build linux && !cgo

package account

import (
	"context"
	"os"
	"os/exec"
	"strconv"
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
func platformHome(uid int) (string, bool) {
	// A read that fails is not an answer: the file is then treated as one holding
	// no entry for uid, so the NSS source can still speak for it.
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		data = nil
	}
	return lookupHome(context.Background(), string(data), uid, runGetent)
}

// runGetent asks getent(1) for uid's passwd entry, and reports whether it
// answered. The paths are absolute because PATH is environment, and a lookup that
// is the daemon's whole account anchor must not be steerable by a caller's PATH.
//
// A getent that exits non-zero — an unknown key, most often — is not an answer,
// so the second path is tried and a failure of both is reported as one.
func runGetent(ctx context.Context, uid int) (string, bool) {
	for _, path := range [...]string{"/usr/bin/getent", "/bin/getent"} {
		out, err := exec.CommandContext(ctx, path, "passwd", strconv.Itoa(uid)).Output()
		if err == nil {
			return string(out), true
		}
	}
	return "", false
}
