//go:build !unix

package cli

import "os"

// ownedByCurrentUser cannot be answered off unix, and "cannot answer" must not
// read as "yes": a recorded attach path is then simply not trusted, and the
// launcher falls back to its own sockets. Plumb does not ship a daemon on these
// platforms at all (the locks are flock(2)), so this is belt and braces.
func ownedByCurrentUser(os.FileInfo) bool { return false }
