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
