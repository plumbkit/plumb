package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/stats"
)

// aliasedProject builds one project reachable by two spellings and returns them.
// The alias is constructed rather than relying on the platform's own (macOS
// /tmp → /private/tmp), so these mean the same thing on Linux.
func aliasedProject(t *testing.T) (realRoot, alias string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	realRoot = filepath.Join(base, "real", "proj")
	if err := os.MkdirAll(filepath.Join(realRoot, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	alias = filepath.Join(base, "alias", "proj")
	if filepath.Clean(alias) == filepath.Clean(realRoot) {
		t.Fatalf("fixture is not aliased: both clean to %q", realRoot)
	}
	return realRoot, alias
}

// TestPeerArea_AliasedPathStillRendersAnArea guards one of the five consumers
// that broke when the workspace root became canonical while agent-supplied paths
// stayed raw (issue #263). ws is the resolved pin; absPath is a path recorded
// from whatever spelling the peer used. A lexical compare renders an empty area,
// so session_start's peer digest silently loses the line.
func TestPeerArea_AliasedPathStillRendersAnArea(t *testing.T) {
	realRoot, alias := aliasedProject(t)

	got := peerArea(context.Background(), realRoot, filepath.Join(alias, "internal", "auth", "token.go"), nil)
	if want := "internal/auth/"; got != want {
		t.Errorf("peerArea(aliased) = %q, want %q", got, want)
	}
	// A write genuinely outside the project must still render nothing.
	outside := filepath.Join(filepath.Dir(filepath.Dir(realRoot)), "elsewhere", "x.go")
	if got := peerArea(context.Background(), realRoot, outside, nil); got != "" {
		t.Errorf("peerArea(outside) = %q, want empty", got)
	}
}

// TestGitCommitRepo_AliasedRepoIsRelativised covers the same shape in the git
// write digest: `repo` is the agent's own recorded argument, so it carries the
// spelling the agent used, not the one the pool resolved.
func TestGitCommitRepo_AliasedRepoIsRelativised(t *testing.T) {
	realRoot, alias := aliasedProject(t)
	sub := filepath.Join(alias, "internal")

	got := gitCommitRepo(stats.RecentCall{InputJSON: `{"repo":"` + sub + `"}`}, realRoot)
	if got != "internal" {
		t.Errorf("gitCommitRepo(aliased) = %q, want %q — an absolute path here means the "+
			"digest prints the full path instead of the project-relative one", got, "internal")
	}
}
