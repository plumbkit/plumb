//go:build !unix

package account

import "os"

// ownedByUID cannot be answered off unix, and "cannot answer" must not read as
// "yes": the caller then takes the fallback directory instead of trusting one it
// could not check.
func ownedByUID(os.FileInfo, int) bool { return false }
