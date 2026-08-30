package tools

import (
	"fmt"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// ContestedFn reports whether this connection's workspace pin is contested — the
// signature of several agents multiplexing one plumb serve without declaring a
// session_id on either channel (issue #182). When it reports true, a RELATIVE
// path argument is refused at the resolvePath seams, because such a path names
// no root of its own and would be anchored to whichever project currently holds
// the contested pin. nil means never contested.
type ContestedFn func() bool

// ContestedRelativePathError is a relative path argument on a connection whose
// workspace pin is contested. It is refused rather than resolved: anchoring a
// relative path to a contested pin silently aims the call at whichever project
// holds the pin right now, which is exactly the wrong-root read and write the
// 2026-08-28 incident produced. An ABSOLUTE path stays admitted — it names a
// project independently of the pin, and the ordinary boundary guard then checks
// it against the currently-pinned workspace.
type ContestedRelativePathError struct {
	Path string
}

func (e ContestedRelativePathError) Error() string {
	return fmt.Sprintf(
		"%s is a relative path, and this connection's workspace pin is contested: "+
			"several agents are multiplexing this plumb serve without declaring an identity, so a relative path "+
			"cannot be attributed to any one of them and would be anchored to whichever project holds the pin right now. "+
			"Use an ABSOLUTE path instead — it is checked against the currently-pinned workspace's boundary, so it can "+
			"never silently land in the wrong project. To stop the pin from being shared, pass session_start.session_id "+
			"on every call, or run one plumb serve per agent.",
		e.Path,
	)
}

// WithContested (on ReadFile) lives here rather than in read_file.go to keep that
// file under the ~600-line size cap; it wires the same contested-pin reporter as
// every other path-bearing read tool.
func (t *ReadFile) WithContested(fn ContestedFn) *ReadFile {
	t.contested = fn
	return t
}

// classifyContestedRelative attaches the workspace-boundary classification to a
// ContestedRelativePathError. The remedy is fixing the argument (pass an
// absolute path), NOT re-pinning — on a contested connection re-pinning is the
// very fight this refusal exists to defuse.
func classifyContestedRelative(err error) error {
	if err == nil {
		return nil
	}
	return toolerror.New(toolerror.KindWorkspaceBoundary, err, toolerror.Remediation{
		Class:  toolerror.ClassFixArguments,
		Reason: "Re-issue the call with an absolute path: on a contested connection a relative path cannot be attributed to a project.",
	})
}
