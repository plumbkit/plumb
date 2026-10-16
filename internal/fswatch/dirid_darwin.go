//go:build darwin

package fswatch

import (
	"os"
	"syscall"
)

// birthTime is the directory's birth time, which darwin's stat reports.
func birthTime(_ string, fi os.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Birthtimespec.Sec*1e9 + st.Birthtimespec.Nsec, true
}
