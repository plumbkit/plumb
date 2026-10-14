//go:build !linux && !darwin

package account

import (
	"os/user"
	"strconv"
)

// platformAccount is the account lookup for the platforms plumb does not ship a
// daemon on. It uses the OS account database and nothing else; a failure is a
// refusal, never a fallback to the environment.
func platformAccount(uid int) (name, home string, ok bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u == nil || u.HomeDir == "" {
		return "", "", false
	}
	return u.Username, u.HomeDir, true
}
