//go:build unix

package account

import (
	"os"
	"syscall"
)

// ownedByUID reports whether info describes a file owned by uid. It is the same
// check internal/paths makes before trusting a runtime directory, and it is
// deliberately narrow: an error, a non-unix stat or a mismatch is false, and the
// caller then falls back rather than trusting the directory.
func ownedByUID(info os.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == uid
}
