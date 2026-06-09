package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// readBoundaryGuard and writeBoundaryGuard are the per-connection BoundaryGuard
// closures wired into every path-bearing tool. They share one PathPolicy but
// demand different access: reads succeed on any allowed root (workspace,
// configured extra roots, configured read roots, and the Go dependency cache);
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
// applyProjectConfig — see conn.go) and refreshed off-lane with the Go
// dependency roots by warmDepRoots, so the guard never builds on read. Returns
// nil while the session is unattached (the guards then no-op).
func (s *connSession) boundaryPolicy() *tools.PathPolicy {
	return s.view().policy
}

// buildPathPolicy assembles the allowlist for v's pinned workspace: the
// workspace (read-write), configured extra roots (read-write), configured read
// roots (read-only), and — for a Go session with dependency reads enabled — the
// Go toolchain's module cache and GOROOT (read-only, from v.depRoots, which
// warmDepRoots populates off the mutation lane). Returns nil when no workspace is
// pinned. Call only from within a mutate fn — it reads the snapshot being built.
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
	if v.ws.AllowDependencyReads && v.acquiredLanguage == "go" {
		roots = append(roots, v.depRoots...)
	}
	return tools.NewPathPolicy(ws, roots)
}

// warmDepRoots computes the Go toolchain's read-only dependency roots off the
// mutation lane and folds them into the session's PathPolicy once known. The
// eager policy built on attach excludes them (so attach never blocks on
// `go env`); this one extra mutate from the warm goroutine rebuilds the policy
// with dep roots. No-op for a non-Go session or when no roots resolve. Dep roots
// are workspace-independent (GOMODCACHE/GOROOT are global), so a re-pin to
// another Go project reuses whatever a prior warm already folded in.
func (s *connSession) warmDepRoots(language string) {
	if language != "go" {
		return
	}
	go func() {
		roots := computeGoDependencyRoots(s.ctx)
		if len(roots) == 0 {
			return
		}
		s.mutate(func(v *sessionView) {
			v.depRoots = roots
			v.policy = s.buildPathPolicy(v)
		})
	}()
}

// computeGoDependencyRoots resolves GOMODCACHE and GOROOT (via `go env`, with
// environment/runtime fallbacks) and returns those that exist as read-only
// roots. Never blocks for long: the `go env` call is bounded by a short
// timeout, and a missing `go` binary degrades to the fallbacks.
func computeGoDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	gomodcache, goroot := goEnvRoots(ctx)
	var roots []tools.AllowedRoot
	add := func(path, label string) {
		if path == "" {
			return
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			roots = append(roots, tools.AllowedRoot{Path: path, Access: tools.AccessRead, Label: label})
		}
	}
	add(gomodcache, "GOMODCACHE")
	add(goroot, "GOROOT")
	return roots
}

func goEnvRoots(ctx context.Context) (gomodcache, goroot string) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "go", "env", "GOMODCACHE", "GOROOT").Output(); err == nil {
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) >= 1 {
			gomodcache = strings.TrimSpace(lines[0])
		}
		if len(lines) >= 2 {
			goroot = strings.TrimSpace(lines[1])
		}
	}
	if goroot == "" {
		goroot = os.Getenv("GOROOT")
	}
	if gomodcache == "" {
		if v := os.Getenv("GOMODCACHE"); v != "" {
			gomodcache = v
		} else if gp := os.Getenv("GOPATH"); gp != "" {
			gomodcache = filepath.Join(gp, "pkg", "mod")
		}
	}
	return gomodcache, goroot
}
