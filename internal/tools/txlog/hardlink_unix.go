//go:build unix

package txlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// refuseMultiplyLinked reports an error when path exists and more than one
// directory entry points at its inode.
//
// A snapshot is a file this daemon wrote into its own tx-log directory moments
// before crashing, so it has exactly one link. A second link means someone else
// made it, and the file being read is theirs, not ours — a hardlink to
// ~/.ssh/id_ed25519 is an ordinary regular file to Lstat, so the symlink refusal
// does not see it.
//
// A missing path is fine: the caller handles the absent-snapshot case.
func refuseMultiplyLinked(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot determine the link count: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown stat representation: this check cannot answer, and "could not
		// check" must not read as "checked and fine".
		return errors.New("cannot determine the link count on this platform")
	}
	if st.Nlink > 1 {
		return fmt.Errorf("file has %d hard links", st.Nlink)
	}
	return nil
}
