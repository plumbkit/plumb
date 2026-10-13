//go:build !linux && !darwin

package account

import (
	"os/user"
	"strconv"
)

// platformHome is the account lookup for the platforms plumb does not ship a
// daemon on. It uses the OS account database and nothing else; a failure is a
// refusal, never a fallback to the environment.
func platformHome(uid int) (string, bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u == nil || u.HomeDir == "" {
		return "", false
	}
	return u.HomeDir, true
}
