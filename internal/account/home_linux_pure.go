//go:build linux && !cgo

package account

import "os"

// platformHome reads uid's home directory from /etc/passwd.
//
// This build cannot ask os/user: without cgo, its linux implementation is a stub
// that answers from $USER and $HOME (go/src/os/user/lookup_stubs.go), so a
// missing passwd entry would come back as a confident answer built from the
// environment — the one thing this package must never accept. Reading the file
// is the authoritative alternative, and a uid it does not contain is a refusal
// rather than a guess.
//
// The cost is explicit: on a host whose accounts come only from NSS (LDAP,
// SSSD), a CGO_ENABLED=0 build refuses to run and says so. A cgo build works
// there.
func platformHome(uid int) (string, bool) {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "", false
	}
	return parsePasswdHome(string(data), uid)
}
