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

// TestWriteSessionPeers_Gating asserts the digest is omitted when the [collab]
// accessor is unset or reports peer_awareness off — the disable-cleanly contract.
func TestWriteSessionPeers_Gating(t *testing.T) {
	ws := t.TempDir()

	t.Run("nil collab accessor", func(t *testing.T) {
		var st SessionStart
		var sb strings.Builder
		st.writeSessionPeers(&sb, ws)
		if sb.Len() != 0 {
			t.Errorf("no collab accessor must write nothing, got %q", sb.String())
		}
	})

	t.Run("peer_awareness off", func(t *testing.T) {
		st := SessionStart{collabFn: func() (bool, int) { return false, 512 }}
		var sb strings.Builder
		st.writeSessionPeers(&sb, ws)
		if sb.Len() != 0 {
			t.Errorf("peer_awareness=false must write nothing, got %q", sb.String())
		}
	})
}
