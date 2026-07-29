//go:build !unix

package fsync

// syncdir_other.go is the fallback for non-Unix platforms (Windows is
// unsupported until 1.1): directory fsync is a no-op.

func syncDir(string) error { return nil }
