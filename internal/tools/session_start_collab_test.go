package tools

import (
	"strings"
	"testing"
)

// TestFormatCollabPolicy is PLAN-301 D4: session_start states the resolved
// collab policy so an agent with peers active learns the mailbox/intents/
// cross-project/findings gates up front, mirroring formatGitPolicy.
func TestFormatCollabPolicy(t *testing.T) {
	tests := []struct {
		name        string
		policy      CollabPolicy
		wantContain []string
	}{
		{
			name:   "everything off except mailbox (the shipped default)",
			policy: CollabPolicy{Mailbox: true},
			wantContain: []string{
				"mailbox on", "intents off", "cross-project off", "findings off",
			},
		},
		{
			name:   "everything on",
			policy: CollabPolicy{Mailbox: true, Intents: true, CrossProject: true, KnowledgeHandoff: true},
			wantContain: []string{
				"mailbox on", "intents on", "cross-project on", "findings on",
			},
		},
		{
			name:        "mailbox off",
			policy:      CollabPolicy{},
			wantContain: []string{"mailbox off"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCollabPolicy(tc.policy)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("want %q in:\n%s", want, got)
				}
			}
		})
	}
}

// TestWriteSessionCollabPolicy_NilSafe guards the unwired case: no mailboxFn
// means no [collab] snapshot exists to report, so the section must be
// omitted rather than panic.
func TestWriteSessionCollabPolicy_NilSafe(t *testing.T) {
	var st SessionStart
	var sb strings.Builder
	st.writeSessionCollabPolicy(&sb, t.TempDir())
	if sb.Len() != 0 {
		t.Errorf("expected no output with mailboxFn unwired; got %q", sb.String())
	}
}

// TestWriteSessionCollabPolicy_SilentWithoutPeers pins the other half of the
// gate. The nil-safe case above short-circuits on mailboxFn before the peer
// check is ever reached, so on its own it would pass with that check deleted:
// this one wires the snapshot and leaves nobody to collaborate with, where a
// solo session gets no section about a policy that governs nothing yet.
func TestWriteSessionCollabPolicy_SilentWithoutPeers(t *testing.T) {
	var st SessionStart
	st.WithMailbox(func() (bool, Inbox) {
		return true, Inbox{Policy: CollabPolicy{Mailbox: true}}
	})
	var sb strings.Builder
	// No session is registered against a freshly made directory, so activePeers
	// finds none there.
	st.writeSessionCollabPolicy(&sb, t.TempDir())
	if sb.Len() != 0 {
		t.Errorf("a session alone on the workspace needs no collab section; got %q", sb.String())
	}
}
