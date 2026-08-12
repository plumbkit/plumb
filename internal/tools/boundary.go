package tools

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// BoundaryGuard rejects paths outside the workspace pinned to this MCP
// connection. A nil guard is a no-op, preserving simple unit-test setup.
type BoundaryGuard func(path string) error

// PinProvenance records how, when, and from where the connection's workspace
// pin was last set. The zero value means unknown, and renders nothing.
type PinProvenance struct {
	Source   string    // "session_start", "roots", "unknown"; "restore:"-prefixed when replayed on reconnect; "" renders nothing
	At       time.Time // when the pin was set; zero omits the age
	Previous string    // previously pinned root; "" on a first attach
}

// String renders the pin provenance for splicing into a boundary-error
// message or daemon_info output. An empty Source is the zero value's
// signature and renders "" so callers can unconditionally append the result
// without an extra empty-check producing a stray space. A "restore:"-prefixed
// Source is rendered as prose ("via X, restored on reconnect") — the raw
// colon-delimited label belongs in log fields, not an agent-facing sentence.
func (p PinProvenance) String() string {
	if p.Source == "" {
		return ""
	}
	source, restored := strings.CutPrefix(p.Source, "restore:")
	var sb strings.Builder
	sb.WriteString("Pin provenance: set")
	if !p.At.IsZero() {
		sb.WriteString(" " + humaniseAge(time.Since(p.At)) + " ago")
	}
	sb.WriteString(" via " + source)
	if restored {
		sb.WriteString(", restored on reconnect")
	}
	if p.Previous != "" {
		sb.WriteString(" (previously " + p.Previous + ")")
	}
	sb.WriteString(".")
	return sb.String()
}

type WorkspaceBoundaryError struct {
	Workspace    string
	Path         string
	ReadOnlyRoot string        // non-empty when the path is under a read-only root; indicates a write was attempted
	Provenance   PinProvenance // pin provenance for the different-project denial; zero value renders nothing (ReadOnlyRoot denials never carry it)
}

// UnattachedWorkspaceError is returned when a path-bearing tool is called on a
// connection with no pinned workspace. plumb refuses such a call rather than
// resolving the path: with no workspace there is no allowlist to check against,
// and a relative path would be resolved by the OS against the daemon's working
// directory — a singleton process whose cwd belongs to whichever client happened
// to spawn it, i.e. an unrelated repository. Fail closed: a refused call is
// recoverable, a misplaced write is not.
type UnattachedWorkspaceError struct {
	Path string
}

func (e UnattachedWorkspaceError) Error() string {
	return fmt.Sprintf(
		"no workspace is pinned to this connection, so %s was refused rather than resolved. "+
			"Call session_start with `workspace` set to an absolute project root to pin this connection, then retry. "+
			"If this session was working a moment ago, the daemon may have restarted and the pin was not re-established.",
		e.Path,
	)
}

func (e WorkspaceBoundaryError) Error() string {
	if e.ReadOnlyRoot != "" {
		return fmt.Sprintf(
			"path access denied: %s is under a read-only root (%s) and cannot be modified. "+
				"Dependency source is not editable; copy the file into your workspace to make changes.",
			e.Path, e.ReadOnlyRoot,
		)
	}
	msg := fmt.Sprintf(
		"workspace boundary violation: this connection is pinned to %s; %s is in a different project. "+
			"To work there, call session_start with workspace set to that project's root — it will re-pin this connection "+
			"(if the re-pin is refused because an explicit session_start pin already holds this connection, retry with force: true). "+
			"Do not browse other projects on disk.",
		e.Workspace, e.Path,
	)
	if prov := e.Provenance.String(); prov != "" {
		msg += " " + prov
	}
	return msg
}

func (g BoundaryGuard) check(path string) error {
	if g == nil || path == "" {
		return nil
	}
	return g(path)
}

func cleanToolPath(path string) string {
	return paths.URIToPath(path)
}

func NewWorkspaceBoundaryError(workspace, path string) error {
	return ClassifyPathRefusal(WorkspaceBoundaryError{Workspace: workspace, Path: path})
}

// IsWorkspaceBoundaryError reports whether err (or anything wrapped in it via
// %w) is a path-access refusal — either a WorkspaceBoundaryError (the path lies
// outside the connection's allowed roots) or an UnattachedWorkspaceError (there
// are no allowed roots because nothing is pinned). Callers use it to suppress a
// fallback that would re-attempt the same refused path. All call sites wrap with
// %w, so errors.As alone is the contract — do not add a substring fallback, as
// it would false-positive on unrelated errors that happen to echo the message.
func IsWorkspaceBoundaryError(err error) bool {
	var boundaryErr WorkspaceBoundaryError
	if errors.As(err, &boundaryErr) {
		return true
	}
	var unattachedErr UnattachedWorkspaceError
	if errors.As(err, &unattachedErr) {
		return true
	}
	var traversalErr ParentTraversalError
	return errors.As(err, &traversalErr)
}

// PathWithinWorkspace reports whether path stays inside workspace after best
// effort canonicalisation. It follows symlinks for existing paths and for the
// nearest existing ancestor, so a symlink inside the workspace cannot be used to
// escape the boundary when creating a new file below it.
//
// An absolute path carrying an unresolved ".." is NOT within any workspace, no
// matter where it appears to clean to: see ParentTraversalError for why the two
// readings of such a path are not interchangeable.
func PathWithinWorkspace(workspace, path string) bool {
	if workspace == "" || path == "" {
		return true
	}
	if hasParentTraversal(path) {
		return false
	}
	return withinRoot(canonicalRoot(workspace), canonicalRoot(path))
}

// canonicalRoot resolves path for boundary comparison. Both PathPolicy roots
// and candidate paths pass through it, so matching is always on resolved paths.
//
// It is the boundary seam's adapter over paths.Canonical, and adds exactly one
// thing: tool arguments may arrive as a file:// URI, which paths.Canonical does
// not know about (it is called with plain paths from inside the daemon too).
// Everything else — following symlinks, resolving the nearest existing ancestor
// for a path that does not exist yet, degrading to a lexical clean when nothing
// resolves — is the shared canonicaliser's, so "are these the same place?" has
// ONE answer here and at workspace-root identity (issue #273).
//
// Two behaviours it deliberately does NOT have, both of which it used to:
//
//   - It does not anchor a relative path with filepath.Abs. A relative path
//     names no location, and the daemon's working directory belongs to whichever
//     client happened to spawn it — resolving against it is the silent
//     cross-repository write of issue #181. An unanchored relative path matches
//     no root and is refused, which is the fail-closed direction.
//   - It does not Clean before resolving. Clean collapses "link/.." as a pair
//     while the kernel follows `link` first, so cleaning first lets two paths
//     naming DIFFERENT directories canonicalise to one string (issue #264).
//
// It is an identity function, not an authorisation check: the refusal of an
// unresolved ".." lives in requireCanonicalPath, ABOVE this, and must stay
// there — canonicalising such a path would silently retarget the operation.
func canonicalRoot(path string) string {
	return paths.Canonical(cleanToolPath(path))
}

// withinRoot reports whether the already-canonicalised path lies within root
// (or is root itself). Both arguments must be resolved by canonicalRoot first.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ParentTraversalError is an absolute path argument carrying an unresolved
// ".." component.
//
// Such a path is refused rather than cleaned, because its two readings are not
// equivalent and plumb must not pick one silently. Lexical cleaning — what
// filepath.Abs does, and therefore what the boundary check ruled on — cancels
// "sub/.." as a pair. The kernel resolves left to right: it follows `sub`
// first, and applies ".." to wherever that landed. When `sub` is a symlink the
// two disagree, so the check and the syscall are about different files and the
// check's verdict says nothing about the file that gets touched.
//
// Cleaning instead would keep every call working while silently retargeting the
// operation to a different file than the caller named. Refusing states the
// ambiguity and costs the caller one edit.
type ParentTraversalError struct {
	Path      string
	Canonical string // the lexically cleaned form, offered as the fix
}

func (e ParentTraversalError) Error() string {
	return fmt.Sprintf(
		"path access denied: %s is not in canonical form. Pass %s instead — an unresolved "+
			"\"..\" makes the boundary check and the filesystem disagree about which file "+
			"the path names, so plumb refuses it rather than guessing. If you meant a path "+
			"reached through a symlink, name the target directly.",
		e.Path, e.Canonical,
	)
}

// hasParentTraversal reports whether an absolute path contains a ".."
// component. Only absolute paths are examined: a relative argument is anchored
// with filepath.Join before it reaches any boundary check, and Join cleans, so
// the anchored result is the single path both the check and the operation use —
// there is no divergence to refuse. Rejecting relative ".." as well would turn
// the ordinary "src/../README.md" into an error for no safety gain.
func hasParentTraversal(path string) bool {
	p := cleanToolPath(path)
	if !filepath.IsAbs(p) {
		return false
	}
	return slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

// requireCanonicalPath refuses an absolute path with an unresolved "..".
// It is the single gate; PathPolicy.Check and PathWithinWorkspace both consult
// it so a new PathPolicy consumer inherits the refusal rather than re-deriving
// it.
func requireCanonicalPath(path string) error {
	if !hasParentTraversal(path) {
		return nil
	}
	p := cleanToolPath(path)
	return toolerror.New(
		toolerror.KindWorkspaceBoundary,
		ParentTraversalError{Path: p, Canonical: filepath.Clean(p)},
		toolerror.Remediation{
			Class: toolerror.ClassFixArguments,
			Reason: "Re-issue the call with the canonical absolute path — one with no \"..\" " +
				"segment — or with a workspace-relative path, which plumb anchors safely.",
		},
	)
}
