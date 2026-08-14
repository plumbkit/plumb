package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

func TestPeerArea_NilStore(t *testing.T) {
	cases := []struct {
		abs  string
		want string
	}{
		{"/ws/internal/tools/x.go", "internal/tools/"},
		{"/ws/main.go", "(root)"},
		{"/other/x.go", ""}, // outside ws
	}
	for _, tc := range cases {
		if got := peerArea(context.Background(), "/ws", tc.abs, nil); got != tc.want {
			t.Errorf("peerArea(%q) = %q, want %q", tc.abs, got, tc.want)
		}
	}
}

func TestFormatPeerDigest(t *testing.T) {
	ws := t.TempDir() // no stats rows for this workspace ⇒ no areas
	now := time.Now()
	peers := []session.Info{
		{ID: "p1", Name: "swift-falcon", ClientName: "claude-code", Folder: ws, LastSeenAt: now.Add(-2 * time.Minute)},
		{ID: "p2", Name: "codex-otter", Folder: ws, LastSeenAt: now.Add(-5 * time.Minute)},
	}
	var st SessionStart
	out := st.formatPeerDigest(ws, peers)
	for _, want := range []string{"## Active peers", "swift-falcon", "[claude-code]", "codex-otter", "observed writes"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing %q:\n%s", want, out)
		}
	}
}

func TestWriteSessionPeers_PolicyAlwaysVisibleWhenPeerActive(t *testing.T) {
	ws := t.TempDir()
	peers := []session.Info{{ID: "peer", Name: "swift-falcon", Folder: ws}}

	t.Run("nil collab accessor", func(t *testing.T) {
		var st SessionStart
		var sb strings.Builder
		st.writeSessionPeersFor(&sb, ws, peers)
		if sb.Len() != 0 {
			t.Errorf("no collab accessor must write nothing, got %q", sb.String())
		}
	})

	t.Run("peer awareness off still shows policy", func(t *testing.T) {
		st := SessionStart{collabFn: func() (bool, int, CollabPolicy) {
			return false, 512, CollabPolicy{Mailbox: true}
		}}
		var sb strings.Builder
		st.writeSessionPeersFor(&sb, ws, peers)
		out := sb.String()
		if !strings.Contains(out, "collab: mailbox ON, intents OFF, cross-project OFF, findings OFF") {
			t.Errorf("active-peer policy missing: %q", out)
		}
		if strings.Contains(out, "## Active peers") {
			t.Errorf("peer_awareness=false leaked the detailed digest: %q", out)
		}
	})
}
