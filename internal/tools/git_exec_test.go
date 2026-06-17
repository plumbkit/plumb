package tools

import (
	"strings"
	"testing"
)

// TestFindGitRoot_EmptyPathRefuses is the last line of defence against the
// cross-session leak: an empty path must error, never resolve to the daemon's
// cwd (os.Getwd), which is shared across connections and may belong to an
// unrelated repository.
func TestFindGitRoot_EmptyPathRefuses(t *testing.T) {
	_, err := findGitRoot("")
	if err == nil || !strings.Contains(err.Error(), "no repository path") {
		t.Fatalf("findGitRoot(\"\") must refuse, got %v", err)
	}
}

// TestEnhanceGitError_SubmodulePathspec covers the submodule-aware rewrite: a
// write that names a path inside a nested submodule must be redirected to the
// submodule's own repo root, with the original message preserved.
func TestEnhanceGitError_SubmodulePathspec(t *testing.T) {
	msg := "fatal: Pathspec 'plumb/site/index.html' is in submodule 'plumb'"
	got := enhanceGitError("/work/ops", msg)
	if !strings.Contains(got, msg) {
		t.Fatalf("hint must preserve the original message, got %q", got)
	}
	for _, want := range []string{"separate repository", "/work/ops/plumb", "repo="} {
		if !strings.Contains(got, want) {
			t.Errorf("want hint to contain %q, got %q", want, got)
		}
	}
}

// TestEnhanceGitError_IndexLockUnaffected proves the refactor left the existing
// stale-lock rewrite firing.
func TestEnhanceGitError_IndexLockUnaffected(t *testing.T) {
	msg := "fatal: Unable to create '/r/.git/index.lock': File exists"
	if got := enhanceGitError("/r", msg); !strings.Contains(got, "leftover lock") {
		t.Errorf("index.lock hint should still fire, got %q", got)
	}
}

// TestEnhanceGitError_UntrackedPathspec covers the freshly-created-file case: a
// path-limited commit naming an untracked path must be redirected to staging it
// first, with the original message preserved.
func TestEnhanceGitError_UntrackedPathspec(t *testing.T) {
	msg := "error: pathspec 'docs/PLAN-00041.md' did not match any file(s) known to git"
	got := enhanceGitError("/r", msg)
	if !strings.Contains(got, msg) {
		t.Fatalf("hint must preserve the original message, got %q", got)
	}
	for _, want := range []string{"not yet tracked", "docs/PLAN-00041.md", "add"} {
		if !strings.Contains(got, want) {
			t.Errorf("want hint to contain %q, got %q", want, got)
		}
	}
}

// TestEnhanceGitError_UntrackedNotSubmodule proves the untracked hint does not
// hijack the submodule failure (both are pathspec errors, distinct markers).
func TestEnhanceGitError_UntrackedNotSubmodule(t *testing.T) {
	msg := "fatal: Pathspec 'plumb/site/index.html' is in submodule 'plumb'"
	if got := enhanceGitError("/work/ops", msg); strings.Contains(got, "not yet tracked") {
		t.Errorf("submodule error must not get the untracked hint, got %q", got)
	}
}

// TestEnhanceGitError_Passthrough proves an unrelated error is returned verbatim.
func TestEnhanceGitError_Passthrough(t *testing.T) {
	msg := "fatal: not a git repository"
	if got := enhanceGitError("/r", msg); got != msg {
		t.Errorf("unrelated error must pass through unchanged, got %q", got)
	}
}

func TestFirstQuoted(t *testing.T) {
	cases := map[string]string{
		"is in submodule 'plumb'": "plumb",
		"no quotes here":          "",
		"unterminated 'open":      "",
	}
	for in, want := range cases {
		if got := firstQuoted(in); got != want {
			t.Errorf("firstQuoted(%q) = %q, want %q", in, got, want)
		}
	}
}
