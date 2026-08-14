package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
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

// TestPeerAreas_OnlyLandedWrites drives peerAreas through the real stats
// query: the digest presents areas as observed writes (facts), so a refused
// edit and a read-tier git call must contribute nothing — only the area of
// the write that landed may appear.
func TestPeerAreas_OnlyLandedWrites(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()
	now := time.Now()
	calls := []stats.Call{
		{
			SessionID: "p1", Workspace: ws, Tool: "edit_file", CalledAt: now,
			Success: false, InputJSON: `{"file_path":"` + ws + `/refuseddir/a.go"}`,
		},
		{
			SessionID: "p1", Workspace: ws, Tool: "git", CalledAt: now,
			Success: true, InputJSON: `{"subcommand":"status"}`,
		},
		{
			SessionID: "p1", Workspace: ws, Tool: "edit_file", CalledAt: now,
			Success: true, InputJSON: `{"file_path":"` + ws + `/landeddir/b.go"}`,
		},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var st SessionStart
	areas := st.peerAreas(ws)
	got := strings.Join(areas["p1"], ", ")
	if strings.Contains(got, "refuseddir") {
		t.Errorf("a refused write contributed an 'observed write' area: %q", got)
	}
	if !strings.Contains(got, "landeddir") {
		t.Errorf("the landed write's area is missing: %q", got)
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
