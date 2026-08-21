//go:build !unix

package paths

import "io/fs"

// ownedByCurrentUser has no portable answer off unix: there is no st_uid to
// compare. windows never reaches it (xdgRuntimeDir returns early), but js,
// wasip1 and plan9 do, so this returns the conservative answer — false sends
// the caller to the cache-dir fallback, which is correct on any platform with
// no XDG_RUNTIME_DIR convention to honour.
func ownedByCurrentUser(fs.FileInfo) bool { return false }
