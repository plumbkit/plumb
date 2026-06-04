package tools

// Access is the permission level a PathPolicy root grants. Roots are additive:
// a path is allowed at the highest access of any root that contains it.
type Access int

const (
	// AccessNone is the zero value; a root with AccessNone is dropped on
	// construction and never matches.
	AccessNone Access = iota
	// AccessRead permits reads/searches only.
	AccessRead
	// AccessReadWrite permits reads and writes.
	AccessReadWrite
)

// AllowedRoot is one entry in a PathPolicy: a canonicalised directory and the
// access it grants. Label is a short human tag surfaced in annotations and
// errors ("workspace", "GOMODCACHE", "configured", …).
type AllowedRoot struct {
	Path   string
	Access Access
	Label  string
}

// PathPolicy is the per-connection allowlist enforced by the read and write
// boundary guards. It generalises the single-workspace boundary: every path a
// tool touches must fall under a root that grants at least the requested
// access. Construction canonicalises every root (symlink- and firmlink-aware
// via canonicalRoot), so matching is done on resolved paths and an in-allowlist
// symlink cannot be used to escape.
//
// The load-bearing invariant: dependency/read roots are added with AccessRead,
// so a write demanding AccessReadWrite can never resolve to one of them — writes
// outside the writable roots fail by construction.
//
// Concurrency: a PathPolicy is immutable after NewPathPolicy. The connection
// rebuilds a fresh one on workspace re-pin or config change rather than
// mutating in place.
type PathPolicy struct {
	primary string // the workspace root, named in the boundary-violation error
	roots   []AllowedRoot
}

// NewPathPolicy canonicalises primary and roots and returns an immutable policy.
// primary is the workspace root named in the boundary-violation message; it
// should also appear in roots (typically with AccessReadWrite). Roots with an
// empty path or AccessNone are dropped.
func NewPathPolicy(primary string, roots []AllowedRoot) *PathPolicy {
	canon := make([]AllowedRoot, 0, len(roots))
	for _, r := range roots {
		if r.Path == "" || r.Access == AccessNone {
			continue
		}
		canon = append(canon, AllowedRoot{Path: canonicalRoot(r.Path), Access: r.Access, Label: r.Label})
	}
	return &PathPolicy{primary: canonicalRoot(primary), roots: canon}
}

// match returns the longest-prefix root containing path (regardless of access)
// and whether one was found. path is canonicalised here.
func (p *PathPolicy) match(path string) (AllowedRoot, bool) {
	if p == nil || path == "" {
		return AllowedRoot{}, false
	}
	cand := canonicalRoot(path)
	var best AllowedRoot
	bestLen := -1
	for _, r := range p.roots {
		if len(r.Path) > bestLen && withinRoot(r.Path, cand) {
			best, bestLen = r, len(r.Path)
		}
	}
	return best, bestLen >= 0
}

// Check resolves path and returns the longest-prefix root that contains it,
// provided that root grants at least want. A nil policy or empty path is
// allowed (no-op), preserving unattached/test setups.
//
// Two distinct denials are possible:
//   - Path falls under a known root, but that root is read-only and a write
//     was requested: returns WorkspaceBoundaryError with ReadOnlyRoot set.
//   - Path falls under no allowed root at all: returns the generic
//     WorkspaceBoundaryError naming the primary workspace.
func (p *PathPolicy) Check(path string, want Access) (AllowedRoot, error) {
	if p == nil || path == "" {
		return AllowedRoot{}, nil
	}
	r, ok := p.match(path)
	if ok && r.Access >= want {
		return r, nil
	}
	cleaned := cleanToolPath(path)
	if ok {
		// Matched a root, but its access level is insufficient (e.g. write to a read-only dep root).
		return AllowedRoot{}, WorkspaceBoundaryError{Workspace: p.primary, Path: cleaned, ReadOnlyRoot: r.Label}
	}
	return AllowedRoot{}, NewWorkspaceBoundaryError(p.primary, cleaned)
}

// OutsideWorkspaceLabel returns the matched root's label when path resolves
// under a non-workspace root (a dependency/read root or a configured extra
// root), and "" when path is inside the workspace or unmatched. Tools use it to
// annotate out-of-workspace reads so the agent knows the content is not
// editable. A nil policy returns "".
func (p *PathPolicy) OutsideWorkspaceLabel(path string) string {
	if p == nil {
		return ""
	}
	r, ok := p.match(path)
	if !ok || r.Label == "workspace" {
		return ""
	}
	return r.Label
}

// ReadGuard and WriteGuard derive BoundaryGuard closures from the policy. The
// daemon prefers live per-call closures (the policy is rebuilt on re-pin), but
// these are convenient for tests and static wiring.
func (p *PathPolicy) ReadGuard() BoundaryGuard {
	return func(path string) error { _, err := p.Check(path, AccessRead); return err }
}

func (p *PathPolicy) WriteGuard() BoundaryGuard {
	return func(path string) error { _, err := p.Check(path, AccessReadWrite); return err }
}
