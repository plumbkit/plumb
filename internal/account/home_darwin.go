//go:build darwin

package account

import (
	"os/user"
	"strconv"
)

// platformAccount asks darwin's account database for uid's entry: its login
// name and home directory.
//
// os/user is libc-backed on darwin even with CGO_ENABLED=0, so it answers from
// the directory service rather than from the environment — which is the whole
// difference from os.UserHomeDir, which reads $HOME.
func platformAccount(uid int) (name, home string, ok bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u == nil || u.HomeDir == "" {
		return "", "", false
	}
	// The lookup must be for the uid we asked about: a mismatch means the
	// account database answered a different question than the one posed.
	if u.Uid != strconv.Itoa(uid) {
		return "", "", false
	}
	// A nameless entry is still an account: the home is what the daemon's
	// directories are built from, and USER is then simply not set.
	return u.Username, u.HomeDir, true
}
