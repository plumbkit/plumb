package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/tools"
)

// The three session_start lines issue #546 item 3 is about, as they render.
const (
	goWorkLine  = "Go LSP:   runs with GOWORK=off — /base/go.work"
	warmingLine = "Language server is still warming up"
	pullLine    = "LSP is ready (diagnostics: pull)"
)

// agentLSPStateConn is one connection shared by a coordinator and a subagent,
// over a pool holding the two Go servers of a go.work + worktree layout: the
// main checkout's (ready, pull diagnostics, workspace mode) and the worktree's
// (still warming, started with GOWORK=off). No server process is started —
// the entries are installed directly, as an acquired pair would leave them.
type agentLSPStateConn struct {
	s           *connSession
	start       *tools.SessionStart
	main, wt    string
	coord, subC context.Context
}

func newAgentLSPStateConn(t *testing.T, connRoot, subRoot func(main, wt string) string) *agentLSPStateConn {
	t.Helper()
	mainDir, wtDir := goWorkWorktreeFixture(t)
	mainDir, wtDir = paths.Canonical(mainDir), paths.Canonical(wtDir)
	mustGitDir(t, mainDir)
	mustGitDir(t, wtDir)

	pool := detectTestPool()
	installEntryLang(pool, mainDir, "go", &stubClient{id: "main"})
	pool.entries[poolKey{mainDir, "go"}].diagMode = diagModePull
	pool.entries[poolKey{wtDir, "go"}] = &poolEntry{
		root: wtDir, language: "go", proxy: &clientProxy{},
		startedAt: time.Now().Add(-5 * time.Second), goWorkOff: "/base/go.work",
	}

	store, ss := newOriginStore(t)
	s := newConnSession(context.Background(), pool, nil, store, nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.onProxySession("proxy-546")

	if _, err := s.repinWorkspace(context.Background(), connRoot(mainDir, wtDir), "", false, false); err != nil {
		t.Fatalf("connection pin: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentCall("subagent")
	coord := mcp.WithLogicalAgent(context.Background(), "coordinator")
	sub := mcp.WithLogicalAgent(context.Background(), "subagent")
	if _, err := s.repinWorkspace(sub, subRoot(mainDir, wtDir), "", true, false); err != nil {
		t.Fatalf("subagent pin: %v", err)
	}
	if got, want := s.workspaceFor(sub), subRoot(mainDir, wtDir); got != want {
		t.Fatalf("subagent workspace = %q, want %q", got, want)
	}
	if got, want := s.workspaceFor(coord), connRoot(mainDir, wtDir); got != want {
		t.Fatalf("coordinator workspace = %q, want %q", got, want)
	}

	// The LSP-state wiring of registerAllTools, which
	// TestSessionStartLSPStateWiredPerAgent pins against the source.
	start := tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithLSPLanguage(s.acquiredLanguageName).
		WithLSPWarmup(s.lspWarmingIn).
		WithLSPDiagMode(s.lspDiagModeIn).
		WithLSPGoWorkOff(s.lspGoWorkOffIn)
	return &agentLSPStateConn{s: s, start: start, main: mainDir, wt: wtDir, coord: coord, subC: sub}
}

func (c *agentLSPStateConn) orient(t *testing.T, ctx context.Context, detail string) string {
	t.Helper()
	out, err := c.start.Execute(ctx, json.RawMessage(`{"detail":"`+detail+`"}`))
	if err != nil {
		t.Fatalf("session_start: %v", err)
	}
	return out
}

// assertLines checks each of the three lines is present exactly when want says
// so. The warm-up and diagnostics-mode lines are in the full packet only.
func assertLines(t *testing.T, who, out string, want map[string]bool) {
	t.Helper()
	for line, on := range want {
		if got := strings.Contains(out, line); got != on {
			t.Errorf("%s: %q present = %v, want %v:\n%s", who, line, got, on, out)
		}
	}
}

// TestSessionStart_LSPStateIsPerAgent is issue #546 item 3, in both
// directions. The GOWORK=off, warming and diagnostics-mode lines describe the
// server serving the CALLING agent's workspace: a subagent pinned to a
// worktree sees its worktree server's GOWORK=off on a connection pinned to the
// main checkout, and a subagent on the main checkout is not told it runs with
// GOWORK=off because the connection is pinned to the worktree.
func TestSessionStart_LSPStateIsPerAgent(t *testing.T) {
	worktreeServer := map[string]bool{goWorkLine: true, warmingLine: true, pullLine: false}
	mainServer := map[string]bool{goWorkLine: false, warmingLine: false, pullLine: true}
	main := func(m, _ string) string { return m }
	wt := func(_, w string) string { return w }

	t.Run("subagent in the worktree, connection on the main checkout", func(t *testing.T) {
		c := newAgentLSPStateConn(t, main, wt)
		assertLines(t, "subagent", c.orient(t, c.subC, "full"), worktreeServer)
		assertLines(t, "coordinator", c.orient(t, c.coord, "full"), mainServer)
		if brief := c.orient(t, c.subC, "brief"); !strings.Contains(brief, goWorkLine) {
			t.Errorf("subagent brief packet lacks the GOWORK=off line:\n%s", brief)
		}
	})
	t.Run("subagent on the main checkout, connection in the worktree", func(t *testing.T) {
		c := newAgentLSPStateConn(t, wt, main)
		assertLines(t, "subagent", c.orient(t, c.subC, "full"), mainServer)
		assertLines(t, "coordinator", c.orient(t, c.coord, "full"), worktreeServer)
		if brief := c.orient(t, c.subC, "brief"); strings.Contains(brief, "GOWORK") {
			t.Errorf("subagent on the main checkout told it runs with GOWORK=off:\n%s", brief)
		}
	})
}

// TestSessionStartLSPStateWiredPerAgent pins the registration the test above
// copies: session_start's LSP-state accessors must be the per-workspace ones,
// or the per-agent resolution is dead code (issue #546).
func TestSessionStartLSPStateWiredPerAgent(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if body == "" {
		t.Fatal("could not locate registerAllTools in conn_register.go — was it renamed?")
	}
	for _, want := range []string{
		"WithLSPWarmup(s.lspWarmingIn)",
		"WithLSPDiagMode(s.lspDiagModeIn)",
		"WithLSPGoWorkOff(s.lspGoWorkOffIn)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session_start is not registered with %s", want)
		}
	}
}
