//go:build darwin

package account

import (
	"os/user"
	"strconv"
)

// platformHome asks darwin's account database for uid's home directory.
//
// os/user is libc-backed on darwin even with CGO_ENABLED=0, so it answers from
// the directory service rather than from the environment — which is the whole
// difference from os.UserHomeDir, which reads $HOME.
func platformHome(uid int) (string, bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u == nil || u.HomeDir == "" {
		return "", false
	}
	// The lookup must be for the uid we asked about: a mismatch means the
	// account database answered a different question than the one posed.
	if u.Uid != strconv.Itoa(uid) {
		return "", false
	}
	return u.HomeDir, true
}
