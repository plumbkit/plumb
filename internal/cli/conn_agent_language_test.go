package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// TestRepinWorkspace_LanguageOverrideRefusedOnSharedConnection pins the
// honest answer to PLAN-428: a logical agent on a shared connection cannot be
// given its own primary language server, because that binding is
// connection-wide. The override is refused with both real remedies instead of
// being stored on the shard and silently ignored.
func TestRepinWorkspace_LanguageOverrideRefusedOnSharedConnection(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()

	// Two identities: the connection is shared, and every declared call
	// routes to its own shard.
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")

	if _, err := s.repinWorkspace(ctxA, "file://"+root, "", false); err != nil {
		t.Fatalf("agent A pin: %v", err)
	}
	_, err := s.repinWorkspace(ctxA, "file://"+root, "go", false)
	if err == nil {
		t.Fatal("a language override on a shared connection must be refused, not stored and ignored")
	}
	for _, want := range []string{`"go"`, `"agent-a"`, "per connection", "dedicated plumb serve", "Omit the language"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must contain %q, got: %v", want, err)
		}
	}
	// The refusal changed nothing: the agent's pin stands, and the same call
	// without the override is the ordinary no-op.
	if got := s.workspaceFor(ctxA); got != root {
		t.Fatalf("agent A workspace after the refusal = %q, want %q", got, root)
	}
	if _, err := s.repinWorkspace(ctxA, "file://"+root, "", false); err != nil {
		t.Fatalf("the same re-pin without a language must still succeed: %v", err)
	}
}

// TestSameRootLanguageSwitchPreservesTrackers: switching the primary language
// on the SAME root changes no file, so the strict-mode reads, the dirty-guard
// writes and the undo history made before the switch are all still valid and
// must survive it. Only a move to a different root starts clean.
func TestSameRootLanguageSwitchPreservesTrackers(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()

	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	read := filepath.Join(root, "read.go")
	written := filepath.Join(root, "written.go")
	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	s.readTracker.Record(read, when, "sha-1")
	s.writeTracker.Record(written)

	// "go" is active in newPersistSession's pool (see
	// TestStickyPin_LanguageOnlyRepinNotRefused), so this is a real same-root
	// language switch, not a silently ignored override.
	if _, err := s.repinWorkspace(context.Background(), root, "go", false); err != nil {
		t.Fatalf("same-root language switch: %v", err)
	}
	// The WRITE tracker is the load-bearing assertion here. The read half is
	// asserted too, but it cannot fail on its own: persistence is on in this
	// harness, so rehydrateReads restores the entry from the durable store
	// immediately after any Reset. Write tracking and undo history have no such
	// safety net — a reset loses them outright — which is what made this a
	// data-loss bug rather than a slow path.
	if got := s.readTracker.Mtime(read); !got.Equal(when) {
		t.Errorf("a same-root language switch reset the read tracker (mtime %v, want %v)", got, when)
	}
	if !s.writeTracker.Wrote(written) {
		t.Error("a same-root language switch reset the write tracker")
	}

	// The contrast: a move to another root does start clean.
	other := freshTempDir(t)
	mustGitDir(t, other)
	if _, err := s.repinWorkspace(context.Background(), other, "", true); err != nil {
		t.Fatalf("move to another root: %v", err)
	}
	if !s.readTracker.Mtime(read).IsZero() || s.writeTracker.Wrote(written) {
		t.Error("a move to a different root must reset the trackers")
	}
}

// TestSameRootLanguageSwitchPreservesShardTrackers is the same rule on the
// SHARED-connection path — the configuration PLAN-428 is about, and the one
// the connection-level test cannot reach. repinAgent resets on any change, and
// a same-root language change needs no override to occur: repinWorkspaceFrom
// passes Detect's language, so an agent re-orienting with a bare
// session_start after `plumb enable-lsp` took the teardown path and lost its
// dirty-guard write state and undo history.
func TestSameRootLanguageSwitchPreservesShardTrackers(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxyShard")
	defer s.close()

	// Two identities: every declared call now routes to its own shard.
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	if _, err := s.repinWorkspace(ctxA, "file://"+root, "", false); err != nil {
		t.Fatalf("agent A pin: %v", err)
	}

	written := filepath.Join(root, "written.go")
	s.writeTrackerFor(ctxA).Record(written)
	if !s.writeTrackerFor(ctxA).Wrote(written) {
		t.Fatal("precondition: the agent's write tracker should hold the recorded path")
	}

	// A same-root language change, as an ordinary re-pin would produce it. It
	// reports a change — the shard's language really did move — which is what
	// makes keeping the trackers a deliberate exception rather than a no-op.
	changed, refused := s.repinAgent(ctxA, root, "go", sessionstate.PinSourceSessionStart, false)
	if refused != nil {
		t.Fatalf("same-root language change on a shard: %v", refused)
	}
	if !changed {
		t.Fatal("precondition: a same-root language change must report a change, or this test proves nothing")
	}
	if !s.writeTrackerFor(ctxA).Wrote(written) {
		t.Error("a same-root language change reset the agent's write tracker")
	}

	// The contrast: moving the agent to another project does start clean.
	other := freshTempDir(t)
	mustGitDir(t, other)
	if moved, refused := s.repinAgent(ctxA, other, "go", sessionstate.PinSourceSessionStart, true); refused != nil || !moved {
		t.Fatalf("agent move: changed=%v err=%v", moved, refused)
	}
	if s.writeTrackerFor(ctxA).Wrote(written) {
		t.Error("moving an agent to another project must reset its write tracker")
	}
}

// TestShardLanguageOverride_NoOpIsNotRefused: the refusal exists because a
// per-agent primary cannot be honoured — but asking for the primary the
// connection already has changes nothing, and failing a no-op would turn the
// guidance this repo now gives every agent (pass session_id, get a shard) into
// an error on an ordinary re-orientation.
func TestShardLanguageOverride_NoOpIsNotRefused(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxyNoop")
	defer s.close()
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	if _, err := s.repinWorkspace(ctxA, "file://"+root, "", false); err != nil {
		t.Fatalf("agent A pin: %v", err)
	}

	// The wiring: a shard exists, so a DIFFERENT language is refused.
	if err := s.refuseShardLanguageOverride(ctxA, "python"); err == nil {
		t.Error("asking for a different primary on a shard must be refused")
	}
	// … and no shard means no refusal, whatever is asked for.
	if err := s.refuseShardLanguageOverride(context.Background(), "python"); err != nil {
		t.Errorf("an unattributed re-pin has no shard and must not be refused: %v", err)
	}

	// The rule itself, with a primary this harness cannot acquire for real:
	// asking for what the connection already has changes nothing.
	if err := shardLanguageOverrideErr("agent-a", "go", "go"); err != nil {
		t.Errorf("asking for the primary the connection already has must not be refused: %v", err)
	}
	err := shardLanguageOverrideErr("agent-a", "python", "go")
	if err == nil {
		t.Fatal("asking for a different primary must be refused")
	}
	for _, want := range []string{`"python"`, `"agent-a"`, "per connection", "(go)", "dedicated plumb serve"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must contain %q, got: %v", want, err)
		}
	}
}

// TestRepinStickyRemedyFitsTheDashboardAlert bounds how long that remedy may
// grow, because its other reader elides.
//
// The same string is spliced into the HealthMessage the dashboard's Alerts
// widget renders, and capAlertLines (internal/tui/dashboard_alerts.go) keeps
// maxAlertLines = 8 wrapped lines by dropping the MIDDLE and preserving the
// tail. A remedy that runs on therefore loses its own advice and keeps only
// its closing sentence — which here is the `force: true` one, the remedy this
// wording exists to demote, so an operator watching a pin fight would be shown
// nothing but the advice that feeds it.
//
// The budget: 8 lines at a realistic 80-column dashboard, minus the box
// chrome, is about 600 characters for the WHOLE message — and the message also
// carries two absolute project paths. Leaving room for those puts the remedy
// itself at 300.
func TestRepinStickyRemedyFitsTheDashboardAlert(t *testing.T) {
	const budget = 300
	if n := len(repinStickyRemedy); n > budget {
		t.Errorf("repinStickyRemedy is %d chars, over the %d-char budget — the dashboard alert elides its middle, "+
			"so the identity and one-serve-per-agent remedies would be dropped and only `force: true` would survive", n, budget)
	}
	// It must still carry both real remedies, or shortening it has traded one
	// failure for the other.
	for _, want := range []string{"session_start.session_id", "plumb serve"} {
		if !strings.Contains(repinStickyRemedy, want) {
			t.Errorf("repinStickyRemedy no longer names %q", want)
		}
	}
}
