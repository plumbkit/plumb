package account

import (
	"context"
	"time"
)

// getentTimeout bounds the getent lookup. NSS can block rather than fail — an
// unreachable directory server (LDAP, SSSD) is a hang — and this runs on the way
// to starting a daemon, so a source that cannot answer promptly is treated as a
// source that did not answer.
const getentTimeout = 2 * time.Second

// getentRunner asks getent(1) for uid's passwd entry and returns its raw output.
//
// It is a parameter rather than a direct call so the fallback can be exercised
// on any platform: the only production caller is the CGO_ENABLED=0 Linux build,
// which is not the build a developer machine runs, and this fallback is the
// difference between a daemon and none at all on an NSS host.
type getentRunner func(ctx context.Context, uid int) (string, bool)

// getentPaths are the getent(1) binaries, in the order they are tried. Both are
// ABSOLUTE: PATH is environment, and a lookup that is the daemon's whole account
// anchor must not be steerable by a caller's PATH. The list lives here rather
// than in the CGO_ENABLED=0 Linux file that runs it so that "absolute, and tried
// in this order" is a fact of the package the tests can check on every platform.
var getentPaths = [...]string{"/usr/bin/getent", "/bin/getent"}

// getentKeyNotFound is getent(1)'s exit status for "the key is not in the
// database" — status 2, as against 1 for a usage error or an unknown database.
const getentKeyNotFound = 2

// getentExec runs one getent binary and reports what it did: err when it could
// not be run at all, else the output and the status it ran to. It is a parameter
// for the same reason getentRunner is — the exit-status rule below decides
// between a refusal and a second round trip to a name service that can be slow,
// and it must be exercised where the tests run.
type getentExec func(ctx context.Context, path string, uid int) (out string, exit int, err error)

// queryGetent asks uid's passwd entry of the first getent in paths that runs,
// and reports whether one answered.
//
// getentKeyNotFound ENDS the lookup, as a refusal. "No such key" is the name
// service answering the question that was put to it — the uid is not in its
// database — and not a failure to run that another copy of the binary might get
// past. Asking the next path cannot change that answer, because both consult the
// same name service, and it spends the daemon's start-up waiting for a slow
// directory server to say the same thing twice.
//
// Any other non-zero status falls through to the next path: the binary ran but
// did not answer for this uid, and the next one may. A path that cannot be run
// at all is the case the list exists for — getent moved between /usr/bin and
// /bin, and usually only one of them is there. A lookup that ends with no path
// answering is the caller's refusal, not a value to fall back on.
func queryGetent(ctx context.Context, uid int, paths []string, run getentExec) (string, bool) {
	for _, path := range paths {
		out, exit, err := run(ctx, path, uid)
		if err != nil {
			continue
		}
		switch exit {
		case 0:
			return out, true
		case getentKeyNotFound:
			return "", false
		}
	}
	return "", false
}

// lookupHome resolves uid's home directory from passwdData — the contents of a
// passwd file — and, when that file has no entry for uid, from getent.
//
// getent is NSS-authoritative: it asks the same name service the C library does,
// so an account served by LDAP, SSSD, Active Directory, systemd-homed or NIS
// answers here even though /etc/passwd never names it. It is also never
// environment-derived — it reads no $HOME or $USER — so consulting it keeps the
// property this package exists for: the home comes from the OS account database,
// or there is no answer.
//
// The uid field must be the one asked for. parsePasswdHome enforces that, so an
// answer naming a different account is a refusal rather than a home to fall back
// on, and so is an answer with an empty home. Only BOTH sources failing is a
// refusal overall.
func lookupHome(ctx context.Context, passwdData string, uid int, getent getentRunner) (string, bool) {
	if home, ok := parsePasswdHome(passwdData, uid); ok {
		return home, true
	}
	ctx, cancel := context.WithTimeout(ctx, getentTimeout)
	defer cancel()
	out, ok := getent(ctx, uid)
	if !ok {
		return "", false
	}
	return parsePasswdHome(out, uid)
}
