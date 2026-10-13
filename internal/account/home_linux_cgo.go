//go:build linux && cgo

package account

import (
	"os/user"
	"strconv"
)

// platformHome asks the system account database for uid's home directory, via
// the NSS-backed os/user lookup that cgo enables.
//
// With cgo the lookup is honest — it goes through getpwuid_r — which is why this
// is the variant used for a cgo build. The CGO_ENABLED=0 build cannot use it:
// there, os/user's stub reads $USER and $HOME and calls that a lookup, which is
// exactly the environment fallback this package refuses (see home_linux_pure.go).
func platformHome(uid int) (string, bool) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil || u == nil || u.HomeDir == "" {
		return "", false
	}
	if u.Uid != strconv.Itoa(uid) {
		return "", false
	}
	return u.HomeDir, true
}
