//go:build !unix

package paths

import "io/fs"

// ownedByCurrentUser is unix-only. Platforms without st_uid never reach it —
// xdgRuntimeDir returns early on windows — so the conservative answer is the
// one that sends the caller to the cache-dir fallback.
func ownedByCurrentUser(fs.FileInfo) bool { return false }
