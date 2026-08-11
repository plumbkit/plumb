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
	if !d.Collab.Mailbox {
		t.Error("collab.mailbox should default to TRUE — same-project agent messaging is on by default")
	}
	if d.Collab.CrossProject {
		t.Error("collab.cross_project should default to false — reaching another project is opt-in")
	}
	if d.Collab.MaxExchanges != 10 {
		t.Errorf("collab.max_exchanges default = %d, want 10", d.Collab.MaxExchanges)
	}
	if d.Collab.ChatBudgetBytes != 2048 {
		t.Errorf("collab.chat_budget_bytes default = %d, want 2048", d.Collab.ChatBudgetBytes)
	}
	if d.Collab.MaxWaitSeconds != 55 {
		t.Errorf("collab.max_wait_seconds default = %d, want 55", d.Collab.MaxWaitSeconds)
	}
	if d.Collab.IntentTTLMinutes != 120 {
		t.Errorf("collab.intent_ttl_minutes default = %d, want 120", d.Collab.IntentTTLMinutes)
	}
	if d.Collab.KnowledgeHandoff {
		t.Error("collab.knowledge_handoff should default to false (opt-in)")
	}
}

// TestLoadProject_CollabChannelSwitchesAreGlobalOnly is the trust-boundary pin.
//
// A project's .plumb/config.toml is an UNTRUSTED surface — a cloned repository
// ships it — so it must not be able to open an inter-agent channel. cross_project
// is the sharp case: it is deliberately the RECIPIENT's decision, so that a
// sender can never push into a project that has not opted in. That guarantee is
// worthless if the recipient's own repository can flip it, which is exactly what
// a hostile clone would do.
//
// These four flags therefore always take the global value, in BOTH directions:
// a project can neither enable nor disable them.
func TestLoadProject_CollabChannelSwitchesAreGlobalOnly(t *testing.T) {
	const allOn = "[collab]\nintents = true\nmailbox = true\ncross_project = true\nknowledge_handoff = true\n"
	const allOff = "[collab]\nintents = false\nmailbox = false\ncross_project = false\nknowledge_handoff = false\n"

	t.Run("a project cannot enable a channel the user left off", func(t *testing.T) {
		base := Defaults()
		base.Collab.Intents = false
		base.Collab.Mailbox = false
		base.Collab.CrossProject = false
		base.Collab.KnowledgeHandoff = false

		got, err := LoadProject(base, writeCollabProject(t, allOn))
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.CrossProject {
			t.Error("a cloned repo enabling cross_project would defeat the guarantee that " +
				"another project cannot inject text into this one uninvited")
		}
		if got.Collab.Mailbox || got.Collab.Intents || got.Collab.KnowledgeHandoff {
			t.Errorf("a project must not open a channel the user switched off: "+
				"mailbox=%v intents=%v knowledge_handoff=%v",
				got.Collab.Mailbox, got.Collab.Intents, got.Collab.KnowledgeHandoff)
		}
	})

	t.Run("a project cannot disable a channel either", func(t *testing.T) {
		// Forcing to the global value is symmetric on purpose. A project silently
		// closing a channel would make peer coordination fail in ways neither agent
		// could explain, and the switch is the user's to hold in both directions.
		base := Defaults()
		base.Collab.Intents = true
		base.Collab.Mailbox = true
		base.Collab.CrossProject = true
		base.Collab.KnowledgeHandoff = true

		got, err := LoadProject(base, writeCollabProject(t, allOff))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Collab.Intents || !got.Collab.Mailbox || !got.Collab.CrossProject || !got.Collab.KnowledgeHandoff {
			t.Error("the channel switches must take the global value in both directions")
		}
	})
}

// TestLoadProject_CollabTuningStaysProjectOverridable is the other half of the
// contract, and the reason the fix above is scoped to the switches alone: tuning
// cannot OPEN anything. A project saying "smaller hints here" is a legitimate
// per-project preference; one saying "open a cross-agent channel here" is not.
func TestLoadProject_CollabTuningStaysProjectOverridable(t *testing.T) {
	base := Defaults()
	ws := writeCollabProject(t, "[collab]\nintent_ttl_minutes = 30\nhint_budget_bytes = 256\n"+
		"max_exchanges = 3\nchat_budget_bytes = 512\nmax_wait_seconds = 10\npeer_awareness = false\n")
	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"intent_ttl_minutes", got.Collab.IntentTTLMinutes, 30},
		{"hint_budget_bytes", got.Collab.HintBudgetBytes, 256},
		{"max_exchanges", got.Collab.MaxExchanges, 3},
		{"chat_budget_bytes", got.Collab.ChatBudgetBytes, 512},
		{"max_wait_seconds", got.Collab.MaxWaitSeconds, 10},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — tuning must stay project-overridable", c.name, c.got, c.want)
		}
	}
	if got.Collab.PeerAwareness {
		t.Error("peer_awareness must stay project-overridable: it surfaces what the daemon " +
			"already observed in this project and opens no channel")
	}
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
