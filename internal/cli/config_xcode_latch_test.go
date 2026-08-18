package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// config_xcode_latch_test.go pins one asymmetry: an unreadable project config
// falls back to the global config for POLICY, but must not trigger an
// irreversible one-shot ACTION on the way past.
//
// Making the unreadable path fall through (so a re-pin could not carry the
// previous workspace's git tier) also made it reach startXcodeForWorkspace,
// which the earlier `return` had skipped. The lasting damage is on DISK, not in
// the latch: xcodebsp.Configure short-circuits once a buildServer.json exists,
// so one written from the GLOBAL scheme because this project's config failed to
// parse is never regenerated until someone deletes the file.
//
// A policy question has an answer either way, so falling back is right. A
// one-shot side effect taken on a guess cannot be undone, so not taking it is.

func xcodeLatchSession(t *testing.T) *connSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := *getDefaultTestConfig()
	// A real pool: startXcodeForWorkspace returns before touching the latch when
	// the pool is nil, so a nil-pool session could not tell the two paths apart.
	// ensureXcodeBuildServer itself is a no-op for a directory that is not a bare
	// Xcode project, so nothing is spawned.
	return &connSession{ctx: ctx, store: config.NewStore(cfg), pool: newWorkspacePool(ctx, cfg)}
}

// xcodeStartedCount reads the per-connection latch under its mutex.
func (s *connSession) xcodeStartedCount() int {
	s.xcodeStartedMu.Lock()
	defer s.xcodeStartedMu.Unlock()
	return len(s.xcodeStarted)
}

func writeConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyProjectConfig_UnreadableConfigDoesNotBurnTheXcodeLatch(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, "this is not valid toml {{{")

	s := xcodeLatchSession(t)
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	s.applyProjectConfig(root)

	if started := s.xcodeStartedCount(); started != 0 {
		t.Errorf("an unreadable config took the Xcode latch (%d root(s)), so a buildServer.json "+
			"can be written from the GLOBAL scheme — and xcodebsp.Configure never regenerates "+
			"one that exists", started)
	}
}

// TestApplyProjectConfig_ReadableConfigStillStartsXcode is the direction an
// over-eager fix would break: skipping the start on the unreadable path must not
// skip it on the ordinary one.
func TestApplyProjectConfig_ReadableConfigStillStartsXcode(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, "[edits]\nrate_limit_per_minute = 120\n")

	s := xcodeLatchSession(t)
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	s.applyProjectConfig(root)

	if started := s.xcodeStartedCount(); started == 0 {
		t.Error("a readable config no longer reaches startXcodeForWorkspace at all")
	}
}

// TestApplyProjectConfig_FixedConfigStartsXcodeAfterAFailedLoad is the
// end-to-end statement of the bug: the broken load must NOT take the latch, and
// the corrected load must.
//
// The assertion has to be ORDERED, not a final count. An earlier version checked
// only `len(xcodeStarted) != 0` at the end, and an independent review pointed
// out that it passes with the fix reverted too: the broken apply takes the
// latch, the corrected apply then returns at it, and the count is 1 either way.
// Counting cannot distinguish WHICH apply took it, which is the entire
// distinction being claimed — so the check is made between the two.
func TestApplyProjectConfig_FixedConfigStartsXcodeAfterAFailedLoad(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, "broken {{{")

	s := xcodeLatchSession(t)
	s.mutate(func(v *sessionView) { v.acquiredRoot = root })
	s.applyProjectConfig(root)

	if n := s.xcodeStartedCount(); n != 0 {
		t.Fatalf("the BROKEN apply took the latch (%d root(s)); everything after this "+
			"passes for the wrong reason", n)
	}

	// The human fixes the typo; the watcher re-applies.
	writeConfig(t, root, "[edits]\nrate_limit_per_minute = 120\n")
	s.applyProjectConfig(root)

	if n := s.xcodeStartedCount(); n == 0 {
		t.Error("the corrected config never reached the Xcode flow — the failed load " +
			"consumed the only chance this root had")
	}
}
