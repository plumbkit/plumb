package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/toolerror"
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

// TestSessionStartStats_AreTheCallersNamespaces: session_start's most-used
// tools, savings and guard counts come from the caller's namespace only.
func TestSessionStartStats_AreTheCallersNamespaces(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := stats.Open()
	if err != nil {
		t.Fatal(err)
	}
	// The default namespace has tool use, savings and a guard refusal; ns-1 has
	// one plain call of its own (so its stats block renders at all).
	for _, c := range []stats.Call{
		{
			SessionID: "d", Workspace: "/w", Tool: "read_file", CalledAt: time.Now(), Success: true,
			SavingsModelVersion: clientcaps.ModelVersion, CapabilityTokens: 5000,
		},
		{SessionID: "d", Workspace: "/w", Tool: "edit_file", CalledAt: time.Now(), ErrorKind: toolerror.KindUnreadOrStale},
		{SessionID: "n", Namespace: "ns-1", Workspace: "/w", Tool: "list_memories", CalledAt: time.Now(), Success: true},
	} {
		if err := db.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	render := func(ns string) string {
		var sb strings.Builder
		(&SessionStart{namespaceFn: func() string { return ns }}).writeSessionStats(&sb, "/w")
		return sb.String()
	}
	const savings, refusals = "estimated read savings", "guard refusals"
	if out := render(""); !strings.Contains(out, "read_file") || !strings.Contains(out, savings) || !strings.Contains(out, refusals) {
		t.Fatalf("control: the default namespace's own stats, savings and refusals are missing:\n%s", out)
	}
	out := render("ns-1")
	if !strings.Contains(out, "list_memories") {
		t.Fatalf("control: the namespace's own tool use is missing:\n%s", out)
	}
	for _, leaked := range []string{"read_file", savings, refusals} {
		if strings.Contains(out, leaked) {
			t.Fatalf("a namespaced session_start reports the default namespace's %q:\n%s", leaked, out)
		}
	}
}
