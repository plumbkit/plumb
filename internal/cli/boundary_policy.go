package cli

import (
	"os"

	"github.com/plumbkit/plumb/internal/tools"
)

// readBoundaryGuard and writeBoundaryGuard are the per-connection BoundaryGuard
// closures wired into every path-bearing tool. They share one PathPolicy but
// demand different access: reads succeed on any allowed root (workspace,
// configured extra roots, configured read roots, and the session language's
// toolchain dependency roots);
// writes succeed only on read-write roots (workspace + configured extra roots),
// so a write outside the workspace is refused by construction.
func (s *connSession) readBoundaryGuard(path string) error {
	return s.checkBoundary(path, tools.AccessRead)
}

func (s *connSession) writeBoundaryGuard(path string) error {
	return s.checkBoundary(path, tools.AccessReadWrite)
}

// checkBoundary consults the live PathPolicy from the session snapshot. An
// unattached session (no pinned workspace) has a nil policy and allows
// everything, preserving the prior behaviour and nil-safe test setups. A denial
// is recorded as a (sticky, non-terminating) boundary violation for the
// dashboard, exactly as before.
func (s *connSession) checkBoundary(path string, want tools.Access) error {
	pol := s.boundaryPolicy()
	if pol == nil || path == "" {
		return nil
	}
	if _, err := pol.Check(path, want); err != nil {
		s.markBoundaryViolation(err.Error())
		return err
	}
	return nil
}

// outsideWorkspaceLabel returns a short label when path resolves under a
// non-workspace allowed root (a dependency or read root), for annotating
// out-of-workspace reads. "" when inside the workspace, unmatched, or unpinned.
func (s *connSession) outsideWorkspaceLabel(path string) string {
	return s.boundaryPolicy().OutsideWorkspaceLabel(path)
}

// boundaryPolicy returns the connection's PathPolicy from the lock-free snapshot.
// The policy is built eagerly on the mutation path (attach / re-pin /
// applyProjectConfig — see conn.go) and refreshed off-lane with the session
// language's toolchain dependency roots by warmDepRoots, so the guard never
// builds on read. Returns nil while the session is unattached (the guards then
// no-op).
func (s *connSession) boundaryPolicy() *tools.PathPolicy {
	return s.view().policy
}

// buildPathPolicy assembles the allowlist for v's pinned workspace: the
// workspace (read-write), configured extra roots (read-write), configured read
// roots (read-only), and — when dependency reads are enabled and v.depRoots were
// computed for the current session language — the session language's toolchain
// dependency roots (read-only, from v.depRoots, which warmDepRoots populates off
// the mutation lane). The depRootsLang guard prevents a stale cross-language
// leak: after a re-pin to another language, the prior language's roots are not
// admitted until warmDepRoots recomputes them for the new language. Returns nil
// when no workspace is pinned. Call only from within a mutate fn — it reads the
// snapshot being built.
func (s *connSession) buildPathPolicy(v *sessionView) *tools.PathPolicy {
	ws := v.acquiredRoot
	if ws == "" {
		return nil
	}
	roots := []tools.AllowedRoot{{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"}}
	for _, r := range v.ws.ExtraRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessReadWrite, Label: "configured"})
		}
	}
	for _, r := range v.ws.ReadRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessRead, Label: "read-root"})
		}
	}
	if v.ws.AllowDependencyReads && v.depRootsLang == v.acquiredLanguage {
		roots = append(roots, v.depRoots...)
	}
	return tools.NewPathPolicy(ws, roots)
}

// warmDepRoots computes the session language's read-only toolchain dependency
// roots off the mutation lane and folds them into the session's PathPolicy once
// known. The eager policy built on attach excludes them (so attach never blocks
// on a toolchain shell-out); this one extra mutate from the warm goroutine
// rebuilds the policy with dep roots. No-op for a language with no resolver, or
// when no roots resolve. The resolved roots are recorded against the language
// (v.depRootsLang) so buildPathPolicy only admits them while the session stays on
// that language — a cross-language re-pin re-warms.
func (s *connSession) warmDepRoots(language string) {
	resolver, ok := depResolvers[language]
	if !ok {
		return
	}
	go func() {
		roots := resolver(s.ctx)
		if len(roots) == 0 {
			return
		}
		s.mutate(func(v *sessionView) {
			v.depRoots = roots
			v.depRootsLang = language
			v.policy = s.buildPathPolicy(v)
		})
	}()
}
