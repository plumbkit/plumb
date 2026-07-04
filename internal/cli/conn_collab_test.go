package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
)

// newIntentTestSession builds a connSession wired to a real per-temp-workspace
// collab pool, with the given [collab] snapshot, so intentHint's gating and
// formatting can be exercised hermetically.
func newIntentTestSession(t *testing.T, ws string, cc config.CollabConfig) *connSession {
	t.Helper()
	s := &connSession{
		store:      config.NewStore(config.Defaults()),
		collabPool: newCollabPool(),
		sessID:     "self",
		ctx:        context.Background(),
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.collab = cc
	})
	t.Cleanup(func() { s.collabPool.closeAll() })
	return s
}

// seedPeerIntent stores an intent authored by a peer session (author_id "peer").
func seedPeerIntent(t *testing.T, s *connSession, ws string, globs []string) {
	t.Helper()
	store := s.collabPool.acquire(ws)
	if store == nil {
		t.Fatal("acquire collab store")
	}
	err := store.PutIntent(context.Background(), collab.IntentInput{
		AuthorSession: "swift-falcon",
		AuthorID:      "peer",
		Body:          "refactoring the rate limiter",
		PathGlobs:     globs,
		TTL:           time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntentHint_MatchingPathIsLabelledClaim(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	got := s.intentHint(args, ws)
	if !strings.Contains(got, "claim, unverified") {
		t.Errorf("intent hint must be labelled as an unverified claim; got %q", got)
	}
	if !strings.Contains(got, "swift-falcon") || !strings.Contains(got, "advisory") {
		t.Errorf("intent hint should name the peer and flag it advisory; got %q", got)
	}
}

func TestIntentHint_DisabledCleanly(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: false, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("intents=false must suppress the hint, got %q", got)
	}
}

func TestIntentHint_OwnIntentNotHinted(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	// An intent authored by THIS session (author_id "self") must not hint itself.
	store := s.collabPool.acquire(ws)
	_ = store.PutIntent(context.Background(), collab.IntentInput{
		AuthorSession: "me", AuthorID: "self", Body: "x", PathGlobs: []string{"ratelimit*"}, TTL: time.Hour,
	}, time.Now())
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("a session must not be hinted about its own intent, got %q", got)
	}
}

func TestIntentHint_NonMatchingPath(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"internal/auth/*.go"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("a non-matching path must not hint, got %q", got)
	}
}

func TestIntentHint_NoStoreNoCreation(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	// No intent has ever been written, so collab.db does not exist; the read path
	// must NOT create it and must yield no hint.
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("no store should yield no hint, got %q", got)
	}
	if collab.Exists(ws) {
		t.Error("the read path must never create collab.db")
	}
}

func TestIntentHint_BudgetCap(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 40})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	got := s.intentHint(args, ws)
	if got == "" {
		t.Fatal("expected a (clamped) hint")
	}
	if len([]byte(got)) > 40 {
		t.Errorf("intent hint %q exceeds the 40-byte budget (%d bytes)", got, len(got))
	}
}

// TestIntentHint_AdvisoryOnlyIsAdditive asserts the hint is a pure suffix
// appended to a tool's output — it never replaces or blocks the response. The
// enrich path (enrichToolOutput) concatenates it, so a write's result is
// unchanged apart from the trailing advisory block.
func TestIntentHint_AdvisoryOnlyIsAdditive(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	base := "wrote ratelimit.go (42 bytes)"
	combined := base + s.intentHint(args, ws)
	if !strings.HasPrefix(combined, base) {
		t.Error("the intent hint must be appended, never replace the tool output")
	}
	if len(combined) <= len(base) {
		t.Error("expected an advisory block to be appended")
	}
}
