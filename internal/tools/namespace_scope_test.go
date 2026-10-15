package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

func registerNSRow(t *testing.T, folder, ns string) session.Info {
	t.Helper()
	info, err := session.Register(session.Info{Folder: folder, Namespace: ns})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return info
}

// TestWorkspaceSessions_SeesOnlyTheCallersNamespace: the roster of a namespaced
// caller lists its namespace's peers and no one else's (PLAN-520), and the
// default namespace's roster leaves the namespaced session out.
func TestWorkspaceSessions_SeesOnlyTheCallersNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const ws = "/ws/ns-scope"
	self := registerNSRow(t, ws, "ns-1")
	peer := registerNSRow(t, ws, "ns-1")
	outsider := registerNSRow(t, ws, "")

	out, err := NewWorkspaceSessions(func() string { return ws }, func() string { return self.ID }).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, peer.Name) {
		t.Fatalf("control: the namespace's own peer is missing:\n%s", out)
	}
	if strings.Contains(out, outsider.Name) {
		t.Fatalf("a default-namespace session appears in a namespace's roster:\n%s", out)
	}
	def, err := NewWorkspaceSessions(func() string { return ws }, func() string { return outsider.ID }).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(def, self.Name) || strings.Contains(def, peer.Name) {
		t.Fatalf("namespaced sessions appear in the default roster:\n%s", def)
	}
}

// TestSessionStartPeers_SeesOnlyTheCallersNamespace: the peer digest
// session_start shows follows the same rule.
func TestSessionStartPeers_SeesOnlyTheCallersNamespace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const ws = "/ws/ns-peers"
	self := registerNSRow(t, ws, "ns-1")
	peer := registerNSRow(t, ws, "ns-1")
	registerNSRow(t, ws, "")

	got := (&SessionStart{selfSessID: func() string { return self.ID }}).activePeers(ws)
	if len(got) != 1 || got[0].ID != peer.ID {
		t.Fatalf("peers = %+v, want only the namespace's own peer %s", got, peer.Name)
	}
}
