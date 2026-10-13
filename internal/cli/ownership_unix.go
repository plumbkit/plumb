//go:build unix

package cli

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info describes a file this process's user
// owns. It is the check that keeps a recorded attach path from being trusted on
// someone else's evidence: a socket under a plumb runtime root that belongs to
// another user is not a daemon this process may talk to.
//
// An error, a non-unix stat or a mismatch is false, and the caller then falls
// back rather than dialling something it could not vouch for.
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}
