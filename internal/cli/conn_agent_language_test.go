package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
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
