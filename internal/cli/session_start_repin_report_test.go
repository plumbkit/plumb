package cli

// session_start_repin_report_test.go — issue #517: session_start's re-pin
// output says WHICH pin moved (the caller's own, or the connection's and how
// many other agents followed it), what the caller's next relative path
// resolves against, and names the caller's own root in the header.
//
// Every case drives the real tool surface (newSessionStartTool: the
// tools.SessionStart wired to this connSession's resolver and re-pin callback)
// because the defect was the tool guessing from its own view what the daemon
// had decided. Each case runs through both the full and the brief packet.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

func newRepinReportSession(t *testing.T) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

func repinReportRoots(t *testing.T, n int) []string {
	t.Helper()
	roots := make([]string, n)
	for i := range roots {
		roots[i] = freshTempDir(t)
		mustGitDir(t, roots[i])
	}
	return roots
}

// runRepinReport executes session_start through the real wiring with args
// plus the given detail, failing the test on an error.
func runRepinReport(t *testing.T, s *connSession, ctx context.Context, args map[string]any, detail string) string {
	t.Helper()
	full := map[string]any{"detail": detail}
	for k, v := range args {
		full[k] = v
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := newSessionStartTool(s).Execute(ctx, raw)
	if err != nil {
		t.Fatalf("session_start %s: %v", raw, err)
	}
	return out
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q\n%s", w, out)
		}
	}
}

func refuseLines(t *testing.T, out string, refuse ...string) {
	t.Helper()
	for _, r := range refuse {
		if strings.Contains(out, r) {
			t.Errorf("output must not contain %q\n%s", r, out)
		}
	}
}

// An identified agent on a shared connection moves ITS OWN pin: the report
// says so, and the connection's pin is left exactly where it was.
func TestSessionStartRepinReport_AgentScopeOnSharedConnection(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 2)
			rootX, rootY := r[0], r[1]
			if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
				t.Fatalf("connection pin to X: %v", err)
			}
			s.recordLogicalAgentAttach("coordinator")
			s.recordLogicalAgentAttach("sub")
			ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")

			out := runRepinReport(t, s, ctxSub, map[string]any{"workspace": rootY, "force": true}, detail)

			wantLines(t, out,
				"# Workspace: "+rootY+"\n",
				"Re-pinned your pin: "+rootX+" → "+rootY+"\n",
				"Next relative-path call resolves against: "+rootY+"\n",
			)
			refuseLines(t, out, "this connection's pin", "your own pin, not the connection's")
			if got := s.workspace(); got != rootX {
				t.Errorf("connection pin = %q, want it unchanged at %q", got, rootX)
			}
			if got := s.workspaceFor(ctxSub); got != rootY {
				t.Errorf("sub resolves to %q, want %q", got, rootY)
			}
		})
	}
}

// An anonymous default-scope re-pin on a shared connection moves the
// CONNECTION's pin, and every seeded peer shard follows it. The caller must be
// told that other agents moved with it.
func TestSessionStartRepinReport_AnonymousDefaultScopeMovesFollowers(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 2)
			rootX, rootY := r[0], r[1]
			// A roots-origin pin is not sticky, so an unforced anonymous
			// session_start may move it — the path case 1 of the issue takes.
			s.attachWorkspace(context.Background(), "file://"+rootX)
			if got := s.workspace(); got != rootX {
				t.Fatalf("precondition: roots attach = %q, want %q", got, rootX)
			}
			s.recordLogicalAgentAttach("a")
			s.recordLogicalAgentAttach("b")
			ctxA := mcp.WithLogicalAgent(context.Background(), "a")
			ctxB := mcp.WithLogicalAgent(context.Background(), "b")
			// Seed both peers' shards where the connection sits.
			for _, ctx := range []context.Context{ctxA, ctxB} {
				if got := s.workspaceFor(ctx); got != rootX {
					t.Fatalf("precondition: seeded shard at %q, want %q", got, rootX)
				}
			}

			out := runRepinReport(t, s, context.Background(), map[string]any{"workspace": rootY}, detail)

			wantLines(t, out,
				"# Workspace: "+rootY+"\n",
				"Re-pinned this connection's pin: "+rootX+" → "+rootY+" (2 other agents follow it)\n",
				"Next relative-path call resolves against: "+rootY+"\n",
			)
			refuseLines(t, out, "Re-pinned your pin", "your own pin, not the connection's")
			// The count is the truth, not a label: both peers really moved.
			for id, ctx := range map[string]context.Context{"a": ctxA, "b": ctxB} {
				if got := s.workspaceFor(ctx); got != rootY {
					t.Errorf("peer %s resolves to %q, want it to have followed to %q", id, got, rootY)
				}
			}
		})
	}
}

// scope: "connection" from an agent holding a pin of its own moves the
// connection's pin but NOT the agent. The report must name the connection's
// previous root (not the agent's), and both the header and the next-call line
// must name the agent's own root, which is where its relative paths still go.
func TestSessionStartRepinReport_ConnectionScopeFromAgentWithOwnPin(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 3)
			rootX, rootW, rootZ := r[0], r[1], r[2]
			if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
				t.Fatalf("connection pin to X: %v", err)
			}
			s.recordLogicalAgentAttach("own")
			s.recordLogicalAgentAttach("peer")
			ctxOwn := mcp.WithLogicalAgent(context.Background(), "own")
			ctxPeer := mcp.WithLogicalAgent(context.Background(), "peer")
			if _, err := s.repinWorkspace(ctxOwn, rootW, "", true, false); err != nil {
				t.Fatalf("own's pin to W: %v", err)
			}
			if got := s.workspaceFor(ctxPeer); got != rootX {
				t.Fatalf("precondition: peer seeded at %q, want %q", got, rootX)
			}

			out := runRepinReport(t, s, ctxOwn, map[string]any{"workspace": rootZ, "scope": "connection", "force": true}, detail)

			wantLines(t, out,
				"# Workspace: "+rootW+"\n",
				"Re-pinned this connection's pin: "+rootX+" → "+rootZ+" (1 other agent follows it)\n",
				"Next relative-path call resolves against: "+rootW+" (your own pin, not the connection's)\n",
			)
			refuseLines(t, out, "# Workspace: "+rootZ, "Re-pinned your pin", rootW+" → ")
			if got := s.workspace(); got != rootZ {
				t.Errorf("connection pin = %q, want %q", got, rootZ)
			}
			if got := s.workspaceFor(ctxOwn); got != rootW {
				t.Errorf("own resolves to %q, want its own pin %q", got, rootW)
			}
			if got := s.workspaceFor(ctxPeer); got != rootZ {
				t.Errorf("peer resolves to %q, want it to have followed to %q", got, rootZ)
			}
		})
	}
}

// scope: "connection" from an agent that never chose a root of its own: its
// shard sits where the connection seeded it, so it follows the move along with
// its peer. It is the caller, not one of the OTHER agents the count reports,
// and it resolves against the connection's new root.
func TestSessionStartRepinReport_ConnectionScopeFromSeededAgent(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 2)
			rootX, rootZ := r[0], r[1]
			if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
				t.Fatalf("connection pin to X: %v", err)
			}
			s.recordLogicalAgentAttach("seeded")
			s.recordLogicalAgentAttach("peer")
			ctxSeeded := mcp.WithLogicalAgent(context.Background(), "seeded")
			ctxPeer := mcp.WithLogicalAgent(context.Background(), "peer")
			for _, ctx := range []context.Context{ctxSeeded, ctxPeer} {
				if got := s.workspaceFor(ctx); got != rootX {
					t.Fatalf("precondition: seeded shard at %q, want %q", got, rootX)
				}
			}

			out := runRepinReport(t, s, ctxSeeded, map[string]any{"workspace": rootZ, "scope": "connection", "force": true}, detail)

			wantLines(t, out,
				"# Workspace: "+rootZ+"\n",
				"Re-pinned this connection's pin: "+rootX+" → "+rootZ+" (1 other agent follows it)\n",
				"Next relative-path call resolves against: "+rootZ+"\n",
			)
			refuseLines(t, out, "your own pin, not the connection's", "Re-pinned your pin")
			for id, ctx := range map[string]context.Context{"seeded": ctxSeeded, "peer": ctxPeer} {
				if got := s.workspaceFor(ctx); got != rootZ {
					t.Errorf("%s resolves to %q, want it to have followed to %q", id, got, rootZ)
				}
			}
		})
	}
}

// On a single-agent connection the connection IS the agent: even an
// identified caller moves the connection's pin, and nobody follows it.
func TestSessionStartRepinReport_SingleAgentConnection(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 2)
			rootX, rootY := r[0], r[1]
			ctxSolo := mcp.WithLogicalAgent(context.Background(), "solo")
			if _, err := s.repinWorkspace(ctxSolo, rootX, "", false, false); err != nil {
				t.Fatalf("first pin: %v", err)
			}

			out := runRepinReport(t, s, ctxSolo, map[string]any{"workspace": rootY, "force": true}, detail)

			wantLines(t, out,
				"# Workspace: "+rootY+"\n",
				"Re-pinned this connection's pin: "+rootX+" → "+rootY+" (no other agent follows it)\n",
				"Next relative-path call resolves against: "+rootY+"\n",
			)
			refuseLines(t, out, "Re-pinned your pin")
			if got := s.workspace(); got != rootY {
				t.Errorf("connection pin = %q, want %q", got, rootY)
			}

			// Absence control for the banner: naming the root already held
			// moves nothing, so no "Re-pinned" line at all.
			again := runRepinReport(t, s, ctxSolo, map[string]any{"workspace": rootY}, detail)
			refuseLines(t, again, "Re-pinned")
		})
	}
}

// An agent that already holds a pin of its own moves it again. "From" must be
// the agent's own previous root, not the connection's, which it never sat on.
func TestSessionStartRepinReport_AgentScopeFromItsOwnPin(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 3)
			rootX, rootW, rootY := r[0], r[1], r[2]
			if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
				t.Fatalf("connection pin to X: %v", err)
			}
			s.recordLogicalAgentAttach("coordinator")
			s.recordLogicalAgentAttach("own")
			ctxOwn := mcp.WithLogicalAgent(context.Background(), "own")
			if _, err := s.repinWorkspace(ctxOwn, rootW, "", true, false); err != nil {
				t.Fatalf("own's pin to W: %v", err)
			}

			out := runRepinReport(t, s, ctxOwn, map[string]any{"workspace": rootY, "force": true}, detail)

			// Positive control and absence share the prefix, so the absence
			// check is live: the line exists, and names W rather than X.
			wantLines(t, out,
				"Re-pinned your pin: "+rootW+" → "+rootY+"\n",
				"Next relative-path call resolves against: "+rootY+"\n",
			)
			refuseLines(t, out, "Re-pinned your pin: "+rootX)
			if got := s.workspace(); got != rootX {
				t.Errorf("connection pin = %q, want it unchanged at %q", got, rootX)
			}
		})
	}
}

// An agent whose workspace declaration was refused resolves to nothing, so
// session_start takes its unattached path — yet the connection is pinned, and
// a scope: "connection" move from there really moves it. The report must be
// rendered on that path too, and the move must settle the CALLER's refusal
// marker (the identity-stripped move used to clear only the anonymous one),
// so the next-call line is true.
func TestSessionStartRepinReport_ConnectionScopeFromRefusedDeclaration(t *testing.T) {
	for _, detail := range []string{"full", "brief"} {
		t.Run(detail, func(t *testing.T) {
			s := newRepinReportSession(t)
			r := repinReportRoots(t, 3)
			rootX, rootY, rootZ := r[0], r[1], r[2]
			if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
				t.Fatalf("connection pin to X: %v", err)
			}
			s.recordLogicalAgentAttach("coord")
			s.recordLogicalAgentAttach("sub")
			ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")
			if _, err := s.repinWorkspace(ctxSub, rootY, "", false, false); err == nil {
				t.Fatal("precondition: an unforced move of a seeded shard to an unrelated root is refused")
			}
			if _, pending := s.pendingDeclarationFor("sub"); !pending {
				t.Fatal("precondition: sub's declaration is pending")
			}

			out := runRepinReport(t, s, ctxSub, map[string]any{"workspace": rootZ, "scope": "connection", "force": true}, detail)

			wantLines(t, out,
				"# Workspace: "+rootZ+"\n",
				"Re-pinned this connection's pin: "+rootX+" → "+rootZ+" (no other agent follows it)\n",
				"Next relative-path call resolves against: "+rootZ+"\n",
			)
			if _, pending := s.pendingDeclarationFor("sub"); pending {
				t.Error("a successful connection-scoped move left the caller's own refusal marker in place")
			}
			if got := s.workspaceFor(ctxSub); got != rootZ {
				t.Errorf("sub resolves to %q, want %q — the report's next-call line must be true", got, rootZ)
			}
		})
	}
}

// The other half of the settling rule: a connection-scoped move that is
// REFUSED settles nothing, so the caller's refused-declaration marker must
// survive it. The successful twin above is the positive control that clearing
// happens at all.
func TestSessionStartRepinReport_RefusedConnectionScopeKeepsCallerMarker(t *testing.T) {
	s := newRepinReportSession(t)
	r := repinReportRoots(t, 3)
	rootX, rootY, rootZ := r[0], r[1], r[2]
	if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
		t.Fatalf("connection pin to X: %v", err)
	}
	s.recordLogicalAgentAttach("coord")
	s.recordLogicalAgentAttach("sub")
	ctxSub := mcp.WithLogicalAgent(context.Background(), "sub")
	if _, err := s.repinWorkspace(ctxSub, rootY, "", false, false); err == nil {
		t.Fatal("precondition: an unforced move of a seeded shard to an unrelated root is refused")
	}
	// Unforced, against the connection's sticky explicit pin: refused.
	raw, err := json.Marshal(map[string]any{"workspace": rootZ, "scope": "connection"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionStartTool(s).Execute(ctxSub, raw); err == nil {
		t.Fatal("precondition: an unforced connection-scoped move off a sticky pin is refused")
	}
	if _, pending := s.pendingDeclarationFor("sub"); !pending {
		t.Error("a REFUSED connection-scoped move cleared the caller's refused-declaration marker")
	}
	if got := s.workspace(); got != rootX {
		t.Errorf("connection pin = %q, want it unchanged at %q", got, rootX)
	}
}

// A shard restored from the agent's own persisted pin holds a root of its own
// without ever having re-pinned in this daemon (selfPinned stays false). It did
// not follow a connection move off another root, so the report must name its
// restored root as the caller's own pin, not the connection's new one.
func TestSessionStartRepinReport_ConnectionScopeFromRestoredPin(t *testing.T) {
	store, ss := newOriginStore(t)
	r := repinReportRoots(t, 3)
	rootX, rootW, rootZ := r[0], r[1], r[2]
	const proxyID = "proxy-restored"
	if err := ss.UpsertPinForAgent(proxyID, "own", rootW, "", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("seed the agent's persisted pin: %v", err)
	}
	s := newPersistSession(t, store, ss, proxyID)
	if _, err := s.repinWorkspace(context.Background(), rootX, "", false, false); err != nil {
		t.Fatalf("connection pin to X: %v", err)
	}
	s.recordLogicalAgentAttach("own")
	s.recordLogicalAgentAttach("peer")
	ctxOwn := mcp.WithLogicalAgent(context.Background(), "own")
	if got := s.workspaceFor(ctxOwn); got != rootW {
		t.Fatalf("precondition: own's shard restored at %q, want %q", got, rootW)
	}

	out := runRepinReport(t, s, ctxOwn, map[string]any{"workspace": rootZ, "scope": "connection", "force": true}, "brief")

	wantLines(t, out,
		"# Workspace: "+rootW+"\n",
		"Re-pinned this connection's pin: "+rootX+" → "+rootZ,
		"Next relative-path call resolves against: "+rootW+" (your own pin, not the connection's)\n",
	)
	if got := s.workspaceFor(ctxOwn); got != rootW {
		t.Errorf("own resolves to %q, want its restored pin %q", got, rootW)
	}
}
