//go:build unix

package fsync

import "os"

// syncdir_unix.go implements the post-rename directory fsync on Unix. Opening
// a directory with O_RDONLY and Sync()ing it flushes its entries (the
// path→inode links created or removed by rename/create/unlink) to stable
// storage — without it, a crash can resurrect the pre-rename directory even
// though the file data itself was fsynced.

// syncDir opens path and fsyncs it.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
