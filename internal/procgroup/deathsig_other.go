//go:build !linux

package procgroup

import "os/exec"

// DieWithParent is a no-op off Linux. Darwin has no equivalent of
// PR_SET_PDEATHSIG, so after a SIGKILL of the parent a child is reclaimed only
// when it notices its stdin has closed. Callers that spawn servers over stdio
// rely on that and document it as the residual.
func DieWithParent(*exec.Cmd) {}
