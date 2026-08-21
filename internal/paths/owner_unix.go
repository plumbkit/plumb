//go:build unix

package paths

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info's owner is the calling user.
//
// The XDG spec requires this check on $XDG_RUNTIME_DIR before trusting it: the
// directory holds the daemon's socket, so a directory owned by someone else is
// a directory someone else can put a socket in. Anything unexpected is treated
// as "not ours" and sends the caller to the fallback.
func ownedByCurrentUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}
