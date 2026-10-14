//go:build !linux && !darwin

package cli

import "os"

// identityOf cannot be answered on a platform whose change time this build does
// not read, and "cannot answer" must not authorise a removal: the marker is left
// unknown, so the daemon never removes a runtime file there (it leaves litter at
// shutdown rather than deleting a file it cannot prove it created). Plumb ships
// a daemon on linux and darwin only — those two have the identity files that
// carry the change time — so this is belt and braces, the same shape as
// ownership_other.go.
func identityOf(os.FileInfo) fileIdentity { return fileIdentity{} }
