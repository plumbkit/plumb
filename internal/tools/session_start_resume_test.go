package tools

import (
	"strings"
	"testing"
)

// What the packet says about a presented resume credential. The wording is the
// agent's only signal that its identity came back whole, or that its credential was
// refused, so each outcome is pinned and a call that presented nothing is unchanged.
func TestSessionStart_SelfLineReportsAResumeCredential(t *testing.T) {
	with := func(o ResumeOutcome) *SessionStart {
		s := (&SessionStart{}).
			WithSelfIdentity(func() string { return "wise-cobra" }).
			WithSelfSession(func() string { return "ad50278a-rest" })
		s.callLink = LinkResult{InheritedName: "wise-cobra", Credential: o}
		return s
	}

	restored := with(ResumeRestored).selfIdentityLine("wise-cobra")
	for _, want := range []string{"wise-cobra (you", "resumed", "identity restored", "mail and threads"} {
		if !strings.Contains(restored, want) {
			t.Errorf("a restored resume omits %q:\n%q", want, restored)
		}
	}
	if strings.Contains(restored, "new internal identity") || strings.Contains(restored, "NOTE:") {
		t.Errorf("a restored resume is also told it lost its identity:\n%q", restored)
	}

	for outcome, word := range map[ResumeOutcome]string{ResumeSuperseded: "superseded", ResumeRevoked: "revoked"} {
		got := with(outcome).selfIdentityLine("wise-cobra")
		if !strings.Contains(got, "NOTE:") || !strings.Contains(got, word) || !strings.Contains(got, "did not follow you") {
			t.Errorf("a %s credential is not reported plainly:\n%q", outcome, got)
		}
		if strings.Contains(got, "identity restored") {
			t.Errorf("a %s credential claims a restore:\n%q", outcome, got)
		}
	}

	if got := with("").selfIdentityLine("wise-cobra"); strings.Contains(got, "NOTE:") || strings.Contains(got, "identity restored") {
		t.Errorf("a call that presented no credential is described differently:\n%q", got)
	}
}

// A connection that lost the rotation is told on every orientation, not once.
func TestSessionStart_LinkageNoteKeepsTellingASupersededConnection(t *testing.T) {
	note := (&SessionStart{}).WithLinkageState(func() LinkageState {
		return LinkageState{ExternalID: "conversation-abc", Recovery: "superseded"}
	})
	got := note.linkageNote(true)
	if !strings.Contains(got, "same resume credential first") || !strings.Contains(got, "temporary identity") {
		t.Errorf("a superseded connection is not told:\n%q", got)
	}
}
