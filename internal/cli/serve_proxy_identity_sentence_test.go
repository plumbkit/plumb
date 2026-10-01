package cli

// serve_proxy_identity_sentence_test.go — #565: the reconnect note may say an
// identity was RESTORED only when it was.
//
// "recoveryEstablished" is first contact under a credential: an identity was
// minted and durably committed, and nothing was recovered. The note used to
// handle it with recoveryRestored and tell the agent "you are still X" — an agent
// that has never been X, and so does not re-declare itself, re-check its pin or
// look for mail it was never going to get.

import (
	"strings"
	"testing"
)

func TestIdentitySentence_OnlyARestoreSaysRestored(t *testing.T) {
	t.Parallel()

	const name, id = "pale-finch", "4e58e4b2c0d1"
	cases := []struct {
		outcome recoveryOutcome
		// affirms: the sentence claims the identity came back.
		affirms bool
		want    []string
	}{
		{outcome: recoveryRestored, affirms: true, want: []string{"you are still " + name, id}},
		{outcome: recoveryEstablished, want: []string{"started a new plumb session", name, id, "session_start with your session_id"}},
		{outcome: recoveryDegraded, want: []string{"could NOT be restored", "temporary one"}},
		{outcome: recoveryUnavailable, want: []string{"no durable session identity", "new"}},
		{outcome: "", want: []string{"did not report an identity outcome", "unknown"}},
		{outcome: "a-value-from-a-newer-daemon", want: []string{"unknown"}},
	}
	for _, c := range cases {
		t.Run(string(c.outcome), func(t *testing.T) {
			t.Parallel()
			got := reconnectOutcome{recovery: string(c.outcome), name: name, sessionID: id}.identitySentence()
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("%q does not say %q", got, want)
				}
			}
			// The one sentence that may say "restored" affirmatively is the restore's;
			// the degraded one says it NEGATIVELY, which is the opposite claim.
			affirming := strings.ReplaceAll(got, "could NOT be restored", "")
			if claims := strings.Contains(affirming, "restored") || strings.Contains(affirming, "you are still"); claims != c.affirms {
				t.Errorf("outcome %q: claims the identity came back = %v, want %v: %q", c.outcome, claims, c.affirms, got)
			}
		})
	}

	t.Run("an established identity without a name still does not claim a restore", func(t *testing.T) {
		t.Parallel()
		got := reconnectOutcome{recovery: string(recoveryEstablished)}.identitySentence()
		if strings.Contains(got, "restored") || strings.Contains(got, "you are still") {
			t.Errorf("claims a restore: %q", got)
		}
		if !strings.Contains(got, "started a new plumb session") {
			t.Errorf("does not say a new session started: %q", got)
		}
	})
}

// The whole note, as an agent reads it, for the established case.
func TestReconnectNoteText_EstablishedIdentityIsNotCalledRestored(t *testing.T) {
	t.Parallel()
	got := reconnectNoteText("1.2.3", "1.2.3", true, reconnectOutcome{
		instanceKnown: true, restarted: true, recovery: string(recoveryEstablished), name: "pale-finch", sessionID: "4e58e4b2",
	})
	for _, avoid := range []string{"identity was restored", "you are still"} {
		if strings.Contains(got, avoid) {
			t.Errorf("an established identity was reported with %q: %q", avoid, got)
		}
	}
	if !strings.Contains(got, "started a new plumb session (name pale-finch, id 4e58e4b2)") {
		t.Errorf("the note does not name the new identity: %q", got)
	}
}
