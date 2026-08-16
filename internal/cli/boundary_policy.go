package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/tools/txlog"
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

// txlogReplayGuard adapts a freshly built PathPolicy into the guard txlog.Scan
// requires before it will restore a path named in an orphaned transaction
// manifest — a file a cloned repository can ship.
//
// It takes the policy VALUE rather than reading it back through s.view(), and
// that is load-bearing. Every caller runs inside s.mutate, which publishes the
// new snapshot only on return, so s.view() there still yields the PRE-mutation
// one — and on a first attach its policy is nil. Consulting it would fail closed
// on exactly the legitimate crash recovery this exists to permit.
//
// A nil policy yields a nil guard, which txlog.Scan treats as fail-closed. That
// case is unreachable in production: buildPathPolicy returns nil only when no
// root is pinned, and Scan is a no-op for an empty workspace anyway.
func txlogReplayGuard(pol *tools.PathPolicy) txlog.PathGuard {
	if pol == nil {
		return nil
	}
	return func(path string) error {
		_, err := pol.Check(path, tools.AccessReadWrite)
		return err
	}
}

// checkBoundary consults the live PathPolicy from the session snapshot.
//
// An unattached session (no pinned workspace) has a nil policy and FAILS CLOSED:
// the call is refused rather than allowed. It used to be allowed, on the stated
// assumption that an unresolvable relative path would be "rejected honestly" by
// this very guard (see the resolvePath doc comments) — but the guard was disabled
// in exactly that case, so the two safety nets missed each other and the tool ran
// the filesystem operation against the daemon's cwd, i.e. an unrelated project.
// An empty path carries no location to check and stays a no-op.
//
// The unattached refusal is deliberately NOT recorded via markBoundaryViolation:
// it is transient (the next session_start clears it by pinning a workspace),
// whereas the violation flag is sticky and would leave the session showing
// "Health: blocked" long after it attached. A real out-of-bounds path on an
// attached session is still recorded, exactly as before.
func (s *connSession) checkBoundary(path string, want tools.Access) error {
	if path == "" {
		return nil
	}
	pol := s.boundaryPolicy()
	if pol == nil {
		return tools.ClassifyPathRefusal(tools.UnattachedWorkspaceError{Path: path})
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
// builds on read. Returns nil while the session is unattached (checkBoundary
// then refuses every path — fail closed).
func (s *connSession) boundaryPolicy() *tools.PathPolicy {
	return s.view().policy
}

// policyRootRefused reports whether a path policy may NOT be built on the
// pinned root, for either of two reasons.
//
// A workspace root that is not absolute names no location, so no policy can
// be built from it. Refusing here makes checkBoundary answer with
// UnattachedWorkspaceError — which is both the accurate description (nothing
// usable is pinned) and the actionable one ("call session_start with an
// absolute project root").
//
// Both checks live at this CHOKE POINT on purpose. Six paths can set
// acquiredRoot — session_start's re-pin, roots/list at initialize and on
// change, the serve proxy's cwd hint, the auto-attach seed, and synthetic
// re-attach on rehydrate. Guarding the re-pin alone was tried first and an
// independent review demonstrated the same brick still reachable through
// three of the others; guarding a fifth would have left the sixth. Every one
// of them ends up here, so the invariant holds however the root arrived.
//
// Without the absolute check the two halves of the daemon disagree:
// detection resolves a relative seed against the daemon's working directory
// and the pin SUCCEEDS, then NewPathPolicy drops the non-absolute root it
// produced, leaving a policy with no roots that refuses every path —
// including the workspace's own files — while the error names the workspace
// as "." and advises the re-pin that just happened. Refusing to build the
// policy at all turns a bricked session into one clear, correct sentence.
//
// The containment re-check (issue #306) covers what attach-time checks
// structurally cannot: the root-setting writers verified the DIRECTORY at
// attach time, but the pinned STRING is re-canonicalised on every policy
// rebuild — the config poll alone runs every 30 seconds — and between attach
// and any rebuild nothing stops the directory being swapped for a symlink to
// a home-containing one (no race needed: whoever can write the directory's
// parent can swap it). Without this check one rebuild would absorb the swap
// and hand the session a home-wide boundary that nothing ever declared. So
// each build re-asks what the canonical root IS, and an undeclared session
// whose root now contains a home directory gets NO policy: checkBoundary
// then answers every path with UnattachedWorkspaceError — fail closed, the
// posture of no pin at all — and the session is marked blocked so the
// operator can see why. A declared session_start pin is exempt here as
// everywhere in this guard (issue #182).
//
// The exemption asks pinUnverifiedReplay as well as the origin, because a pin
// replayed over the serve proxy's initialize _meta records session_start origin
// while being a claim the daemon cannot authenticate (issue #318). That channel
// is precisely the one best placed to run the swap this re-check exists for —
// its holder names the root, so it can point that directory at a home container
// after a clean attach — and withholding the exemption at attach time while
// granting it here would leave the guard covering the case nobody can reach and
// missing the one somebody can.
func (s *connSession) policyRootRefused(v *sessionView, ws string) bool {
	if !filepath.IsAbs(ws) {
		return true
	}
	declared := v.pinOrigin == sessionstate.PinSourceSessionStart && !v.pinUnverifiedReplay
	if !declared && containsUserHome(paths.Canonical(ws)) {
		s.log().Warn("daemon: refusing to build a path policy — the pinned root now contains a home directory", "root", ws, "origin", string(v.pinOrigin), "unverified_replay", v.pinUnverifiedReplay)
		s.markBoundaryViolation(fmt.Sprintf("workspace root %s now contains a home directory (issue #306); re-pin deliberately with session_start", ws))
		return true
	}
	return false
}

// buildPathPolicy assembles the allowlist for v's pinned workspace: the
// workspace (read-write), configured extra roots (read-write), configured read
// roots (read-only), the trusted per-workspace roots the user granted manually
// (extra read-write / read read-only, from the out-of-repo WorkspaceRootsStore),
// and — when dependency reads are enabled and v.depRoots were
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
	if s.policyRootRefused(v, ws) {
		return nil
	}
	roots := []tools.AllowedRoot{{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"}}
	for _, r := range v.ws.ExtraRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessReadWrite, Label: "configured"})
		}
	}
	// Client-granted extra roots (serve --allow-dir) are additive read-write
	// roots, never replacing the workspace baseline. They arrive already
	// $VAR-expanded and absolute from the serve process; NewPathPolicy then
	// canonicalises each (symlink-/firmlink-aware), so an allow-dir cannot be used
	// to escape via a symlink any more than the workspace root can.
	for _, d := range v.allowDirs {
		if d != "" {
			roots = append(roots, tools.AllowedRoot{Path: d, Access: tools.AccessReadWrite, Label: "allow-dir"})
		}
	}
	for _, r := range v.ws.ReadRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessRead, Label: "read-root"})
		}
	}
	// Trusted per-workspace roots the user granted manually through the TUI /
	// CLI, recorded in plumb's data dir keyed by the workspace root — never in
	// the (untrusted) project config. Additive to the config roots: extra roots
	// read-write, read roots read-only. buildPathPolicy runs only on the mutation
	// lane (attach / re-pin / config reload / warmDepRoots), never per tool call,
	// so reading the small store here is off the hot path.
	granted := config.NewWorkspaceRootsStore().Get(ws)
	for _, r := range granted.ExtraRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessReadWrite, Label: "workspace-root"})
		}
	}
	for _, r := range granted.ReadRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessRead, Label: "workspace-read-root"})
		}
	}
	if v.ws.AllowDependencyReads && v.depRootsLang == v.acquiredLanguage {
		roots = append(roots, v.depRoots...)
	}
	return tools.NewPathPolicy(ws, roots).WithProvenance(tools.PinProvenance{
		Source:   v.pinVia,
		At:       v.pinAt,
		Previous: v.pinPrev,
	})
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
