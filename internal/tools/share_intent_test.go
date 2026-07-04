package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// collabTestDeps builds a CollabDeps backed by a real per-temp-workspace store.
// storeCreated reports whether Store() was ever invoked, so a gating test can
// assert the disabled path never touches (and never creates) collab.db.
func collabTestDeps(t *testing.T, policy CollabPolicy) (CollabDeps, *collab.Store, *bool) {
	t.Helper()
	ws := t.TempDir()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created := false
	deps := CollabDeps{
		Workspace:   func() string { return ws },
		SessionName: func() string { return "test-session" },
		SessionID:   "sess-1",
		Policy:      func() CollabPolicy { return policy },
		Store: func() *collab.Store {
			created = true
			return store
		},
	}
	return deps, store, &created
}

func TestShareIntent_DisabledRefusesCleanly(t *testing.T) {
	deps, _, created := collabTestDeps(t, CollabPolicy{Intents: false, IntentTTLMinutes: 120})
	tool := NewShareIntent(deps)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"refactoring"}`))
	if err != nil {
		t.Fatalf("disabled should not error: %v", err)
	}
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "intents = true") {
		t.Errorf("expected a clear enable hint; got %q", out)
	}
	if *created {
		t.Error("the disabled path must not touch the collab store (no collab.db creation)")
	}
}

func TestShareIntent_EnabledStoresRedactedIntent(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Intents: true, IntentTTLMinutes: 120})
	tool := NewShareIntent(deps)
	// A body carrying a fake secret must be scrubbed before persistence.
	body := `refactoring the limiter api_key=SUPERSECRETVALUE123456`
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":`+jsonStr(body)+`,"path_globs":["internal/tools/ratelimit*"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Intent broadcast") {
		t.Errorf("unexpected confirmation: %q", out)
	}
	intents, err := store.LiveIntents(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Fatalf("stored intents = %d, want 1", len(intents))
	}
	if strings.Contains(intents[0].Body, "SUPERSECRETVALUE") {
		t.Errorf("intent body was persisted UNREDACTED: %q", intents[0].Body)
	}
	if !strings.Contains(intents[0].Body, "REDACTED") {
		t.Errorf("expected a redaction placeholder in %q", intents[0].Body)
	}
	if len(intents[0].PathGlobs) != 1 || intents[0].PathGlobs[0] != "internal/tools/ratelimit*" {
		t.Errorf("path globs not persisted: %v", intents[0].PathGlobs)
	}
}

func TestShareIntent_MissingBodyRejected(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Intents: true})
	tool := NewShareIntent(deps)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected an error for a missing body")
	}
}

func TestShareIntent_NoWorkspace(t *testing.T) {
	tool := NewShareIntent(CollabDeps{
		Workspace:   func() string { return "" },
		SessionName: func() string { return "s" },
		Policy:      func() CollabPolicy { return CollabPolicy{Intents: true} },
		Store:       func() *collab.Store { return nil },
	})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"body":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "session_start") {
		t.Errorf("expected an attach hint; got %q", out)
	}
}

func TestResolveTTL(t *testing.T) {
	if got := resolveTTL(120, 30); got != 30*time.Minute {
		t.Errorf("override should win: got %v", got)
	}
	if got := resolveTTL(120, 0); got != 120*time.Minute {
		t.Errorf("policy default should apply: got %v", got)
	}
	if got := resolveTTL(0, 0); got != defaultIntentTTLMinutes*time.Minute {
		t.Errorf("compiled default should apply: got %v", got)
	}
}

// jsonStr returns s as a JSON string literal for inline test payloads.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
