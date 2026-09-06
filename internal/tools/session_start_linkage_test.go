package tools

// session_start_linkage_test.go — the linkage and recovery notes key on the
// CONNECTION's persisted state, not on what the call in flight declared, and a
// name-only resume says what did not follow it.
//
// The bug these pin: the old unlinked note fired whenever the CALL carried no
// session_id, so a bare re-orientation call from a session that had linked an
// hour ago was told it had no external id — and a degraded connection said
// nothing at all in the packets, even though its reconnect note (one-shot, and
// only on a reconnect) was long gone.

import (
	"strings"
	"testing"
)

func TestSessionStart_LinkageNoteKeysOnPersistedState(t *testing.T) {
	const wantSubstr = "no external id"
	const degradedSubstr = "could not fully apply your proven identity"

	t.Run("unwired falls back to this-call linkage", func(t *testing.T) {
		note := &SessionStart{}
		if got := note.linkageNote(false); !strings.Contains(got, wantSubstr) {
			t.Errorf("unwired + unlinked call: no warning at all:\n%q", got)
		}
		if got := note.linkageNote(true); got != "" {
			t.Errorf("unwired + linked call: got %q, want silence (legacy fallback)", got)
		}
	})

	t.Run("a linked session is never warned, whatever the call carried", func(t *testing.T) {
		note := (&SessionStart{}).WithLinkageState(func() LinkageState {
			return LinkageState{ExternalID: "conversation-abc", Recovery: "restored"}
		})
		// The bare re-orientation call — the exact shape that used to false-warn.
		if got := note.linkageNote(false); got != "" {
			t.Errorf("a linked session was warned on a bare call:\n%q", got)
		}
		if got := note.linkageNote(true); got != "" {
			t.Errorf("a linked session was warned on a linking call:\n%q", got)
		}
	})

	t.Run("an unlinked session is told the cost, not just the gap", func(t *testing.T) {
		note := (&SessionStart{}).WithLinkageState(func() LinkageState {
			return LinkageState{Recovery: "established"}
		})
		got := note.linkageNote(false)
		if !strings.Contains(got, wantSubstr) {
			t.Errorf("the unlinked warning is missing:\n%q", got)
		}
		if !strings.Contains(got, "NEW identity") || !strings.Contains(got, "will not follow you") {
			t.Errorf("the unlinked warning states only the addressability gap, not the fork "+
				"cost a client restart carries:\n%q", got)
		}
	})

	t.Run("degraded says so, and says it before anything else", func(t *testing.T) {
		note := (&SessionStart{}).WithLinkageState(func() LinkageState {
			// Degraded with no external id: the degraded sentence is the one
			// that matters — the recovery, not the linkage, is the live state.
			return LinkageState{Recovery: "degraded"}
		})
		got := note.linkageNote(false)
		if !strings.Contains(got, degradedSubstr) {
			t.Errorf("a degraded connection is not told it is running under a temporary identity:\n%q", got)
		}
		if !strings.Contains(got, "retried automatically") {
			t.Errorf("the degraded note does not say the recovery is retried:\n%q", got)
		}
	})

	t.Run("degraded outranks the unlinked warning", func(t *testing.T) {
		note := (&SessionStart{}).WithLinkageState(func() LinkageState {
			return LinkageState{ExternalID: "conversation-abc", Recovery: "degraded"}
		})
		got := note.linkageNote(true)
		if !strings.Contains(got, degradedSubstr) {
			t.Errorf("a degraded LINKED connection got:\n%q", got)
		}
	})
}

func TestSessionStart_SelfLineDisclosesNameOnlyResume(t *testing.T) {
	linked := func(resumed bool) *SessionStart {
		return (&SessionStart{}).
			WithSelfIdentity(func() string { return "wise-cobra" }).
			WithSelfSession(func() string { return "ad50278a-rest" }).
			WithResumedNewIdentity(func() bool { return resumed })
	}

	line := linked(true).selfIdentityLine("wise-cobra")
	if !strings.Contains(line, "resumed") {
		t.Fatalf("a name-only resume must still say it resumed:\n%q", line)
	}
	if !strings.Contains(line, "new internal identity") || !strings.Contains(line, "not inherited") {
		t.Errorf("a name-only resume does not disclose the new identity or the stranded mail:\n%q", line)
	}

	if got := linked(false).selfIdentityLine("wise-cobra"); strings.Contains(got, "new internal identity") {
		t.Errorf("a non-resuming call discloses a resume that did not happen:\n%q", got)
	}

	// The refused-name case keeps its own suffix: the caller asked for a name
	// it did not get, which outranks the continuation fact.
	line = linked(true).selfIdentityLine("name-in-use")
	if !strings.Contains(line, "requested name-in-use, which is in use") {
		t.Errorf("the refused-name suffix vanished:\n%q", line)
	}
	if strings.Contains(line, "new internal identity") {
		t.Errorf("a refused resume must not advertise the inherited name as resumed:\n%q", line)
	}
}
