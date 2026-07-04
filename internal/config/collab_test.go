package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCollabProject(t *testing.T, body string) string {
	t.Helper()
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCollab_Defaults(t *testing.T) {
	d := Defaults()
	if !d.Collab.PeerAwareness {
		t.Error("collab.peer_awareness should default to true")
	}
	if d.Collab.HintBudgetBytes != 512 {
		t.Errorf("collab.hint_budget_bytes default = %d, want 512", d.Collab.HintBudgetBytes)
	}
	if d.Collab.Intents {
		t.Error("collab.intents should default to false (opt-in)")
	}
	if d.Collab.Mailbox {
		t.Error("collab.mailbox should default to false (opt-in)")
	}
	if d.Collab.IntentTTLMinutes != 120 {
		t.Errorf("collab.intent_ttl_minutes default = %d, want 120", d.Collab.IntentTTLMinutes)
	}
}

// TestLoadProject_CollabPhase2OverridesBothDirections asserts the phase-2 opt-in
// flags follow the same both-directions override rule: a project may ENABLE
// intents/mailbox under a global default-off, and DISABLE them under a global
// opt-in.
func TestLoadProject_CollabPhase2OverridesBothDirections(t *testing.T) {
	t.Run("project enables under global off", func(t *testing.T) {
		base := Defaults() // intents=false, mailbox=false
		ws := writeCollabProject(t, "[collab]\nintents = true\nmailbox = true\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Collab.Intents || !got.Collab.Mailbox {
			t.Errorf("project opt-in must win: intents=%v mailbox=%v", got.Collab.Intents, got.Collab.Mailbox)
		}
	})

	t.Run("project disables under global on", func(t *testing.T) {
		base := Defaults()
		base.Collab.Intents = true
		base.Collab.Mailbox = true
		ws := writeCollabProject(t, "[collab]\nintents = false\nmailbox = false\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.Intents || got.Collab.Mailbox {
			t.Errorf("project opt-out must win: intents=%v mailbox=%v", got.Collab.Intents, got.Collab.Mailbox)
		}
	})

	t.Run("project overrides ttl", func(t *testing.T) {
		ws := writeCollabProject(t, "[collab]\nintent_ttl_minutes = 30\n")
		got, err := LoadProject(Defaults(), ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.IntentTTLMinutes != 30 {
			t.Errorf("intent_ttl_minutes = %d, want 30", got.Collab.IntentTTLMinutes)
		}
	})
}

func TestValidateCollab_NegativeTTLRejected(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nintent_ttl_minutes = -5\n")
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("expected validation error for negative collab.intent_ttl_minutes")
	}
}

// TestLoadProject_CollabOverridesBothDirections asserts the generated_summaries
// precedent for the new [collab] bool: a project may DISABLE peer_awareness under
// a global opt-in, and ENABLE it under a global opt-out. Both win over global.
func TestLoadProject_CollabOverridesBothDirections(t *testing.T) {
	t.Run("project off under global on", func(t *testing.T) {
		base := Defaults() // peer_awareness = true
		ws := writeCollabProject(t, "[collab]\npeer_awareness = false\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.PeerAwareness {
			t.Error("project peer_awareness=false must override global true")
		}
	})

	t.Run("project on under global off", func(t *testing.T) {
		base := Defaults()
		base.Collab.PeerAwareness = false // global opt-out
		ws := writeCollabProject(t, "[collab]\npeer_awareness = true\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Collab.PeerAwareness {
			t.Error("project peer_awareness=true must override global false")
		}
	})

	t.Run("absent key keeps global", func(t *testing.T) {
		base := Defaults()
		base.Collab.PeerAwareness = false
		ws := writeCollabProject(t, "[collab]\nhint_budget_bytes = 256\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.PeerAwareness {
			t.Error("absent peer_awareness must keep the global value (false)")
		}
		if got.Collab.HintBudgetBytes != 256 {
			t.Errorf("hint_budget_bytes = %d, want 256", got.Collab.HintBudgetBytes)
		}
	})
}

func TestValidateCollab_NegativeBudgetRejected(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nhint_budget_bytes = -1\n")
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("expected validation error for negative collab.hint_budget_bytes")
	}
}
