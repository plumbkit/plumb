//go:build linux && !cgo

package account

import (
	"context"
	"errors"
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
// answered. The paths are the absolute getentPaths — never a PATH lookup,
// because PATH is environment and this lookup is the daemon's account anchor —
// and the exit-status rule that turns "no such key" into a refusal lives in
// queryGetent, where every platform's tests can reach it.
func runGetent(ctx context.Context, uid int) (string, bool) {
	return queryGetent(ctx, uid, getentPaths[:], execGetentPath)
}

// execGetentPath runs one getent binary and reports what it did.
//
// A binary that could not be run at all — absent, or killed when the lookup's
// deadline expired — is an error rather than a status, which is what tells
// queryGetent to try the next path. A status means the command RAN: that
// includes the "no such key" status, which is the name service answering rather
// than a reason to ask again.
func execGetentPath(ctx context.Context, path string, uid int) (string, int, error) {
	// G204: path is one of the two absolute getentPaths, this package's own
	// closed list, and the rest of the argv is literal.
	out, err := exec.CommandContext(ctx, path, "passwd", strconv.Itoa(uid)).Output() //nolint:gosec // closed list, literal argv
	if err == nil {
		return string(out), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode(), nil
	}
	return "", 0, err
}
