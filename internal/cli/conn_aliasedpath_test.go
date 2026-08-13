package cli

// conn_aliasedpath_test.go — the consumers that compare the canonical workspace
// root against a path the AGENT supplied (issue #263). Canonicalising the root
// fixes the consumers fed by the pool and BREAKS these, because one side became
// always-resolved while the other stayed whatever spelling the client knows.
// Each failure below is silent: "outside the workspace" is a legitimate answer
// these callers simply drop.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// TestEpisodicRelPath_AliasedPathResolvesInside: an episodic memory records the
// areas a session touched. With a lexical compare the file is dropped, so the
// summary reports no areas at all for an aliased project.
func TestEpisodicRelPath_AliasedPathResolvesInside(t *testing.T) {
	realRoot, alias := aliasedWorkspace(t)

	if got := episodicRelPath(realRoot, filepath.Join(alias, "internal", "auth.go")); got != "internal/auth.go" {
		t.Errorf("episodicRelPath(aliased) = %q, want %q", got, "internal/auth.go")
	}
	// A genuine escape, and the workspace itself, must still yield nothing.
	if got := episodicRelPath(realRoot, filepath.Join(freshTempDir(t), "x.go")); got != "" {
		t.Errorf("episodicRelPath(outside) = %q, want empty", got)
	}
	if got := episodicRelPath(realRoot, realRoot); got != "" {
		t.Errorf("episodicRelPath(the workspace itself) = %q, want empty", got)
	}
}

// TestResolveCLIWorkspaceDetailed_MarkerlessRootIsCanonicalButNotEscaped covers
// the CLI's markerless branch, which had no test at all — the change there
// survived mutation until a review pointed it out.
//
// Two properties: canonical, and never an ancestor the user did not name (this
// value flows into `plumb config unset --workspace`, which would then edit a
// .plumb/config.toml the user never pointed at). Historically this fixture
// also discriminated "must not call SynthesiseRoot" — back when that function
// had no $HOME guard, calling it here walked up to the dotfiles .git. It now
// carries the same guard and stops at $HOME, so on THIS shape the two answers
// coincide; the test that still separates them is
// TestResolveCLIWorkspaceDetailed_HomeItselfIsInspectable below.
func TestResolveCLIWorkspaceDetailed_MarkerlessRootIsCanonicalButNotEscaped(t *testing.T) {
	base := freshTempDir(t)
	realDir := filepath.Join(base, "real", "scratch")
	mustMkdir(t, realDir)
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A dotfiles repo AT $HOME, above the markerless directory: Detect refuses
	// the .git there, so the markerless branch is reached.
	t.Setenv("HOME", base)
	mustGitDir(t, base)

	root, attachable, err := resolveCLIWorkspaceDetailed(filepath.Join(base, "alias", "scratch"), config.Defaults())
	if err != nil {
		t.Fatalf("resolveCLIWorkspaceDetailed: %v", err)
	}
	if root != realDir {
		t.Errorf("root = %q, want %q — the directory the user named, resolved", root, realDir)
	}
	if root == base || root == filepath.Join(base, "real") {
		t.Errorf("root escaped to an ancestor (%q); `plumb config unset --workspace` would "+
			"then edit a .plumb/config.toml the user never named", root)
	}
	if attachable {
		t.Error("a markerless dir must report non-attachable under the default config")
	}
}

// TestResolveCLIWorkspaceDetailed_HomeItselfIsInspectable pins the behaviour
// that still separates the CLI's markerless branch from SynthesiseRoot, now
// that both carry the $HOME guard: the CLI is an INSPECTION surface, so
// `plumb config show --workspace ~` must answer about the home directory the
// user named — while SynthesiseRoot's non-explicit mode refuses a $HOME seed
// (returns "") because for the daemon that seed is an incidental tool path.
// A review showed the previous fixture no longer discriminated: rewiring the
// branch through SynthesiseRoot left every test green. Under this one the
// rewire returns "" (non-explicit) and fails.
func TestResolveCLIWorkspaceDetailed_HomeItselfIsInspectable(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustGitDir(t, home) // dotfiles repo at $HOME: Detect refuses it, reaching the markerless branch

	root, attachable, err := resolveCLIWorkspaceDetailed(home, config.Defaults())
	if err != nil {
		t.Fatalf("resolveCLIWorkspaceDetailed: %v", err)
	}
	if want := paths.Canonical(home); root != want {
		t.Errorf("root = %q, want %q — naming the home directory to an inspection command must answer about it", root, want)
	}
	if attachable {
		t.Error("the home directory must report non-attachable under the default config")
	}
}

// TestRootFromClient_DetectFailureFallbackIsCanonical covers the other change a
// review found untested — it survived mutation because nothing exercised it.
//
// When Detect finds no marker under the root the CLIENT reported, rootFromClient
// falls back to that folder. The value still becomes session_start's answer and
// is compared against peers' session.Folder, which the pool resolved — so a raw
// spelling silently hides every peer in the project.
func TestRootFromClient_DetectFailureFallbackIsCanonical(t *testing.T) {
	base := freshTempDir(t)
	realDir := filepath.Join(base, "real", "bare")
	mustMkdir(t, realDir)
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	alias := filepath.Join(base, "alias", "bare")

	// Precondition: this must really be the Detect-failure branch, or the test
	// would pass through the ordinary (already canonical) path and prove nothing.
	if _, _, err := detectTestPool().Detect(alias); err == nil {
		t.Fatal("fixture resolved a project root; the Detect-failure fallback is not exercised")
	}

	s := newConnSession(context.Background(), detectTestPool(), nil,
		config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	s.setClientRequest(func(context.Context, string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"roots":[{"uri":"file://` + alias + `"}]}`), nil
	})

	if got := s.rootFromClient(context.Background()); got != realDir {
		t.Errorf("rootFromClient = %q, want %q — the reported root must be resolved before "+
			"it is compared against peers' session.Folder", got, realDir)
	}
}
