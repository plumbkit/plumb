package fswatch

import "os"

// dirID is a directory's identity, as worktrees.go compares it: the file
// identity os.SameFile reads (device and inode) and, where the platform
// reports one, the directory's birth time. Device and inode alone are not
// enough: a filesystem such as ext4 hands a freed inode straight to the next
// directory made, so a tree recreated right after `rm -rf` reads as the old one
// (#703's CI caught exactly that). The birth time tells them apart.
type dirID struct {
	fi       os.FileInfo
	birth    int64 // nanoseconds since the epoch; valid when hasBirth
	hasBirth bool
}

// statDir is dirID's os.Lstat.
func statDir(path string) (dirID, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return dirID{}, err
	}
	birth, ok := birthTime(path, fi)
	return dirID{fi: fi, birth: birth, hasBirth: ok}, nil
}

// sameDir reports whether a and b are one directory: the same file, and — when
// both carry a birth time — born at the same moment.
func sameDir(a, b dirID) bool {
	if !os.SameFile(a.fi, b.fi) {
		return false
	}
	return !a.hasBirth || !b.hasBirth || a.birth == b.birth
}
