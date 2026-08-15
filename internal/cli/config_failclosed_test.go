package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// config_failclosed_test.go pins one rule: a project config that plumb cannot
// read means "no project config", never "the previous project's config".
//
// The gate exists because a cloned repository ships a .plumb/config.toml. If an
// unreadable one silently preserves whatever was applied last, a repository
// inherits a git tier it was never granted by shipping BROKEN TOML — which is
// the trust gate inverted, since there is no `plumb trust` against it anywhere.

// trustedWorkspace writes a project config and returns its root.
func trustedWorkspace(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// elevatedSession returns a session whose live view already carries an elevated
// git tier, standing in for "previously pinned to a trusted project".
func elevatedSession(t *testing.T) *connSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}
	s.mutate(func(v *sessionView) {
		v.git.AllowPush = true
		v.git.AllowDestructive = true
	})
	return s
}

// TestApplyProjectConfig_UnparseableConfigDropsThePreviousPolicy is PLAN-309
// reproduction 2, the security-relevant one: a session pinned to a trusted
// workspace, re-pinned into one whose config will not parse, used to carry the
// first project's allow_push into the second.
func TestApplyProjectConfig_UnparseableConfigDropsThePreviousPolicy(t *testing.T) {
	hostile := trustedWorkspace(t, "this is not valid toml {{{")

	s := elevatedSession(t)
	s.mutate(func(v *sessionView) { v.acquiredRoot = hostile })
	s.applyProjectConfig(hostile)

	v := s.view()
	global := s.store.Current()
	if v.git.AllowPush != global.Git.AllowPush {
		t.Errorf("allow_push = %v, want the global %v — the previous project's tier followed the session",
			v.git.AllowPush, global.Git.AllowPush)
	}
	if v.git.AllowDestructive != global.Git.AllowDestructive {
		t.Errorf("allow_destructive = %v, want the global %v", v.git.AllowDestructive, global.Git.AllowDestructive)
	}
	// The agent must still be told why nothing from the file is in effect.
	if !v.projectGit.Unreadable {
		t.Error("projectGit.Unreadable = false; the agent has no way to learn the config was skipped")
	}
}

// TestApplyProjectConfig_UnparseableConfigDropsEveryBlock: the carryover was
// never specific to [git]. Anything the previous project set survived, so the
// fix must revert the whole view, not one block of it. [collab] cross_project
// is the one that matters most after git — it is consent to receive another
// project's mail.
func TestApplyProjectConfig_UnparseableConfigDropsEveryBlock(t *testing.T) {
	hostile := trustedWorkspace(t, "= = =")

	s := elevatedSession(t)
	s.mutate(func(v *sessionView) {
		v.collab.CrossProject = true
		v.edits.Strict = !s.store.Current().Edits.Strict
	})
	s.mutate(func(v *sessionView) { v.acquiredRoot = hostile })
	s.applyProjectConfig(hostile)

	v, global := s.view(), s.store.Current()
	if v.collab.CrossProject != global.Collab.CrossProject {
		t.Errorf("collab.cross_project = %v, want the global %v — stale consent to another project's mail",
			v.collab.CrossProject, global.Collab.CrossProject)
	}
	if v.edits.Strict != global.Edits.Strict {
		t.Errorf("edits.strict = %v, want the global %v", v.edits.Strict, global.Edits.Strict)
	}
}

// TestCheckAndReloadConfig_DeletionRevertsToGlobal: deleting the file must not
// be a way to keep what it granted. Before this, the overrides simply stayed in
// force for the life of the session.
func TestCheckAndReloadConfig_DeletionRevertsToGlobal(t *testing.T) {
	root := trustedWorkspace(t, "[edits]\nrate_limit_per_minute = 999\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })

	s.checkAndReloadConfig()
	if got := s.view().edits.RateLimitPerMinute; got != 999 {
		t.Fatalf("project override did not apply: rate limit = %d, want 999", got)
	}
	if s.view().lastCfgMtime.IsZero() {
		t.Fatal("lastCfgMtime not seeded; the rest of this test would prove nothing")
	}

	if err := os.Remove(filepath.Join(root, ".plumb", "config.toml")); err != nil {
		t.Fatal(err)
	}
	s.checkAndReloadConfig()

	global := s.store.Current()
	if got := s.view().edits.RateLimitPerMinute; got != global.Edits.RateLimitPerMinute {
		t.Errorf("rate limit = %d after deletion, want the global %d", got, global.Edits.RateLimitPerMinute)
	}
	if !s.view().lastCfgMtime.IsZero() {
		t.Error("lastCfgMtime survived the deletion; a reappearing file would be compared against a dead mtime")
	}
}

// TestCheckAndReloadConfig_DeletionIsIdempotent: the revoke happens once. A
// watcher poll runs every few seconds for the life of the session, and
// re-applying on each one would rebuild the path policy forever for a file that
// is simply not there.
func TestCheckAndReloadConfig_DeletionIsIdempotent(t *testing.T) {
	root := trustedWorkspace(t, "[edits]\nrate_limit_per_minute = 999\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	s.checkAndReloadConfig()

	if err := os.Remove(filepath.Join(root, ".plumb", "config.toml")); err != nil {
		t.Fatal(err)
	}
	s.checkAndReloadConfig()
	first := s.view()
	s.checkAndReloadConfig()
	s.checkAndReloadConfig()
	last := s.view()

	if !last.lastCfgMtime.IsZero() {
		t.Error("lastCfgMtime became non-zero on a later poll of a missing file")
	}
	if first.edits.RateLimitPerMinute != last.edits.RateLimitPerMinute {
		t.Errorf("repeated polls changed the applied config: %d then %d",
			first.edits.RateLimitPerMinute, last.edits.RateLimitPerMinute)
	}
}

// TestCheckAndReloadConfig_ReappearingConfigApplies: revoking on deletion must
// not leave the session deaf to the file coming back — a git checkout or a
// restore-from-backup is an ordinary way for that to happen.
func TestCheckAndReloadConfig_ReappearingConfigApplies(t *testing.T) {
	root := trustedWorkspace(t, "[edits]\nrate_limit_per_minute = 999\n")
	configPath := filepath.Join(root, ".plumb", "config.toml")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &connSession{ctx: ctx, store: config.NewStore(*getDefaultTestConfig())}
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	s.checkAndReloadConfig()

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	s.checkAndReloadConfig()

	if err := os.WriteFile(configPath, []byte("[edits]\nrate_limit_per_minute = 777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.checkAndReloadConfig()

	if got := s.view().edits.RateLimitPerMinute; got != 777 {
		t.Errorf("rate limit = %d, want 777 — the restored config was ignored", got)
	}
}
