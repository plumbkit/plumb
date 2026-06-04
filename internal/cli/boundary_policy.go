package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/tools"
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

// checkBoundary consults the live PathPolicy. An unattached session (no pinned
// workspace) has a nil policy and allows everything, preserving the prior
// behaviour and nil-safe test setups. A denial is recorded as a (sticky,
// non-terminating) boundary violation for the dashboard, exactly as before.
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

// boundaryPolicy returns the connection's PathPolicy, rebuilding it when the
// pinned workspace changes. Returns nil while the session is unattached (the
// guards then no-op). The cache is invalidated by applyProjectConfig when
// configured roots may have changed.
func (s *connSession) boundaryPolicy() *tools.PathPolicy {
	ws := s.workspace()
	if ws == "" {
		return nil
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	if s.policy != nil && s.policyRoot == ws {
		return s.policy
	}
	s.policy = s.buildPathPolicy(ws)
	s.policyRoot = ws
	return s.policy
}

// invalidateBoundaryPolicy drops the cached policy so the next guard call
// rebuilds it. Called when configured roots change.
func (s *connSession) invalidateBoundaryPolicy() {
	s.policyMu.Lock()
	s.policy = nil
	s.policyRoot = ""
	s.policyMu.Unlock()
}

// buildPathPolicy assembles the allowlist for ws: the workspace (read-write),
// configured extra roots (read-write), configured read roots (read-only), and
// — for a Go session with dependency reads enabled — the Go toolchain's module
// cache and GOROOT (read-only).
func (s *connSession) buildPathPolicy(ws string) *tools.PathPolicy {
	wc := s.workspaceConfig()
	roots := []tools.AllowedRoot{{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"}}
	for _, r := range wc.ExtraRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessReadWrite, Label: "configured"})
		}
	}
	for _, r := range wc.ReadRoots {
		if p := os.ExpandEnv(r); p != "" {
			roots = append(roots, tools.AllowedRoot{Path: p, Access: tools.AccessRead, Label: "read-root"})
		}
	}
	if wc.AllowDependencyReads && s.languageIsGo() {
		roots = append(roots, s.goDependencyRoots()...)
	}
	return tools.NewPathPolicy(ws, roots)
}

// workspaceConfig returns the session's resolved [workspace] config.
func (s *connSession) workspaceConfig() config.WorkspaceConfig {
	s.wsMu.RLock()
	defer s.wsMu.RUnlock()
	return s.wsCfg
}

// languageIsGo reports whether the pinned workspace attached as a Go project.
func (s *connSession) languageIsGo() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.acquiredLanguage == "go"
}

// warmDepRoots pre-populates the Go dependency roots in the background so the
// first guard check on a Go session does not stall waiting for `go env`. It
// fires at most once per session (sync.Once inside goDependencyRoots ensures
// idempotency). Called immediately after acquiredLanguage is set on both the
// initial attach and re-pin paths.
func (s *connSession) warmDepRoots(language string) {
	if language != "go" {
		return
	}
	go s.goDependencyRoots() // populates depRootsOnce/depRootsVal
}

// goDependencyRoots memoises the Go toolchain's read-only dependency roots for
// the session lifetime. GOMODCACHE/GOROOT are global (workspace-independent),
// so computing them once is correct even across a re-pin; buildPathPolicy gates
// inclusion on the current language and config each rebuild.
func (s *connSession) goDependencyRoots() []tools.AllowedRoot {
	s.depRootsOnce.Do(func() {
		s.depRootsVal = computeGoDependencyRoots(s.ctx)
	})
	return s.depRootsVal
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
