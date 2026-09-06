package config

import "testing"

// TestCollabNoteRetentionDefaults pins the no-change-upgrade defaults: a zero
// note_ttl_minutes follows intent_ttl_minutes — the expiry notes always had —
// and delivered notes keep expiring until a workspace opts into keeping them.
func TestCollabNoteRetentionDefaults(t *testing.T) {
	d := Defaults()
	if d.Collab.NoteTTLMinutes != 0 {
		t.Errorf("collab.note_ttl_minutes default = %d, want 0 (follows intent_ttl_minutes)", d.Collab.NoteTTLMinutes)
	}
	if d.Collab.KeepDeliveredNotes {
		t.Error("collab.keep_delivered_notes should default to false (opt-in)")
	}
}

// TestLoadProject_NoteRetentionStaysProjectOverridable: retention is a
// per-project preference like the other sizes and expiries, not a channel
// switch — a project may set both keys without `plumb trust`.
func TestLoadProject_NoteRetentionStaysProjectOverridable(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nnote_ttl_minutes = 2880\nkeep_delivered_notes = true\n")
	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Collab.NoteTTLMinutes != 2880 {
		t.Errorf("collab.note_ttl_minutes = %d, want 2880", got.Collab.NoteTTLMinutes)
	}
	if !got.Collab.KeepDeliveredNotes {
		t.Error("collab.keep_delivered_notes = false, want the project value true")
	}
}

// TestValidateCollab_NegativeNoteTTLRejected mirrors the intent TTL rule: a
// negative value is a misconfiguration, never a "never expire" instruction —
// permanence is keep_delivered_notes' job, at delivery.
func TestValidateCollab_NegativeNoteTTLRejected(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nnote_ttl_minutes = -5\n")
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("expected validation error for negative collab.note_ttl_minutes")
	}
}

// TestCollabPolicySpec_NoteRetentionNeedsNoTrust: both retention keys must sit
// on the free side of the [collab] allow-list, or a project cannot tune how
// long its own mail is kept without an approval round-trip.
func TestCollabPolicySpec_NoteRetentionNeedsNoTrust(t *testing.T) {
	for _, k := range []string{"note_ttl_minutes", "keep_delivered_notes"} {
		if !policyCollabFreeFields[k] {
			t.Errorf("collab.%s must be trust-free: it tunes retention, not who can send or read", k)
		}
	}
}
