package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/tools"
)

// The session_start lines issue #546 item 3 is about, as they render.
const (
	warmingLine    = "Language server is still warming up"
	pullLine       = "LSP is ready (diagnostics: pull)"
	readyLine      = "LSP is ready"
	notStartedLine = "The Go language server for this workspace has not started yet"
)

// seededGoWork is the go.work a hand-installed worktree entry reports; a
// server the test did not install reports the fixture's real one instead.
const seededGoWork = "/base/go.work"

func runsLine(work string) string { return "Go LSP:   runs with GOWORK=off — " + work }

func willLine(work string) string { return "Go LSP:   will start with GOWORK=off — " + work }

func goWorkLineAny(out string) bool { return strings.Contains(out, "GOWORK") }

// agentLSPStateConn is one connection shared by a coordinator and a subagent
// over a go.work + worktree layout (goWorkWorktreeFixture). The pool holds a
// server for each root in running, installed by hand as an acquired one would
// be left: the main checkout's is ready with pull diagnostics; the worktree's
// is still warming and was started with GOWORK=off. A root not in running has
// no server, exactly as when a subagent's session_start re-pins there: the
// per-agent re-pin starts none.
type agentLSPStateConn struct {
	s           *connSession
	start       *tools.SessionStart
	goWork      string // the fixture's real go.work
	coord, subC context.Context
}

const (
	onMain = iota
	inWorktree
)

func newAgentLSPStateConn(t *testing.T, connAt, subAt int, running ...int) *agentLSPStateConn {
	t.Helper()
	isolateGoWorkEnv(t)
	mainDir, wtDir := goWorkWorktreeFixture(t)
	mainDir, wtDir = paths.Canonical(mainDir), paths.Canonical(wtDir)
	mustGitDir(t, mainDir)
	mustGitDir(t, wtDir)
	root := func(at int) string {
		if at == inWorktree {
			return wtDir
		}
		return mainDir
	}

	pool := detectTestPool()
	for _, at := range running {
		if at == onMain {
			installEntryLang(pool, mainDir, "go", &stubClient{id: "main"})
			pool.entries[poolKey{mainDir, "go"}].diagMode = diagModePull
			continue
		}
		pool.entries[poolKey{wtDir, "go"}] = &poolEntry{
			root: wtDir, language: "go", proxy: &clientProxy{},
			startedAt: time.Now().Add(-5 * time.Second), goWorkOff: seededGoWork,
		}
	}

	store, ss := newOriginStore(t)
	s := newConnSession(context.Background(), pool, nil, store, nil, ss, newSharedBudgets())
	t.Cleanup(s.close)
	s.onProxySession("proxy-546")

	if _, err := s.repinWorkspace(context.Background(), root(connAt), "", false, false); err != nil {
		t.Fatalf("connection pin: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentCall("subagent")
	coord := mcp.WithLogicalAgent(context.Background(), "coordinator")
	sub := mcp.WithLogicalAgent(context.Background(), "subagent")
	if _, err := s.repinWorkspace(sub, root(subAt), "", true, false); err != nil {
		t.Fatalf("subagent pin: %v", err)
	}
	if got := s.workspaceFor(sub); got != root(subAt) {
		t.Fatalf("subagent workspace = %q, want %q", got, root(subAt))
	}
	if got := s.workspaceFor(coord); got != root(connAt) {
		t.Fatalf("coordinator workspace = %q, want %q", got, root(connAt))
	}
	// The premise of the not-started cases: the subagent's re-pin started nothing.
	pool.mu.Lock()
	_, started := pool.entries[poolKey{root(subAt), "go"}]
	pool.mu.Unlock()
	if started && !slices.Contains(running, subAt) {
		t.Fatal("the subagent's re-pin started a server; the not-started case no longer tests its premise")
	}

	// The LSP-state wiring of registerAllTools, which
	// TestSessionStartLSPStateWiredPerAgent pins against the source.
	start := tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithLSPLanguage(s.acquiredLanguageName).
		WithLSPServer(s.lspServerIn).
		WithLSPWarmup(s.lspWarmingIn).
		WithLSPDiagMode(s.lspDiagModeIn).
		WithLSPGoWorkOff(s.lspGoWorkOffIn)
	return &agentLSPStateConn{
		s: s, start: start, coord: coord, subC: sub,
		goWork: filepath.Join(filepath.Dir(mainDir), "go.work"),
	}
}

func (c *agentLSPStateConn) orient(t *testing.T, ctx context.Context, detail string) string {
	t.Helper()
	out, err := c.start.Execute(ctx, json.RawMessage(`{"detail":"`+detail+`"}`))
	if err != nil {
		t.Fatalf("session_start: %v", err)
	}
	return out
}

// assertLines checks each line is present exactly when want says so. The
// warm-up, readiness and diagnostics-mode lines are in the full packet only.
func assertLines(t *testing.T, who, out string, want map[string]bool) {
	t.Helper()
	for line, on := range want {
		if got := strings.Contains(out, line); got != on {
			t.Errorf("%s: %q present = %v, want %v:\n%s", who, line, got, on, out)
		}
	}
}

// TestSessionStart_LSPStateIsPerAgent_ServerNotStarted is the case PR #559's
// review found item 3 missing (B2): a subagent's FIRST session_start in its
// workspace. The per-agent re-pin starts no server there, so nothing may be
// read from one: the GOWORK decision comes from disk, the server is reported
// as not started rather than "ready" (which described the connection's), and
// no warm-up or diagnostics mode is borrowed from the connection's server.
func TestSessionStart_LSPStateIsPerAgent_ServerNotStarted(t *testing.T) {
	t.Run("subagent in the worktree, connection on the main checkout", func(t *testing.T) {
		c := newAgentLSPStateConn(t, onMain, inWorktree, onMain)
		assertLines(t, "subagent", c.orient(t, c.subC, "full"), map[string]bool{
			willLine(c.goWork): true, notStartedLine: true,
			readyLine: false, warmingLine: false, "runs with GOWORK": false,
		})
		if brief := c.orient(t, c.subC, "brief"); !strings.Contains(brief, willLine(c.goWork)) {
			t.Errorf("subagent brief packet lacks the GOWORK=off line:\n%s", brief)
		}
		coord := c.orient(t, c.coord, "full")
		assertLines(t, "coordinator", coord, map[string]bool{pullLine: true, notStartedLine: false})
		if goWorkLineAny(coord) {
			t.Errorf("coordinator on the main checkout told about GOWORK:\n%s", coord)
		}
	})
	t.Run("subagent on the main checkout, connection in the worktree", func(t *testing.T) {
		c := newAgentLSPStateConn(t, inWorktree, onMain, inWorktree)
		sub := c.orient(t, c.subC, "full")
		assertLines(t, "subagent", sub, map[string]bool{notStartedLine: true, readyLine: false, warmingLine: false})
		if goWorkLineAny(sub) {
			t.Errorf("subagent on the main checkout told about GOWORK:\n%s", sub)
		}
		assertLines(t, "coordinator", c.orient(t, c.coord, "full"), map[string]bool{
			runsLine(seededGoWork): true, warmingLine: true,
		})
	})
}

// TestSessionStart_LSPStateIsPerAgent_ServerRunning is item 3 once both
// servers run: each agent is told its own server's GOWORK, warm-up state and
// diagnostics mode, never the connection's.
func TestSessionStart_LSPStateIsPerAgent_ServerRunning(t *testing.T) {
	worktreeServer := map[string]bool{runsLine(seededGoWork): true, warmingLine: true, pullLine: false, notStartedLine: false}
	mainServer := map[string]bool{runsLine(seededGoWork): false, warmingLine: false, pullLine: true, notStartedLine: false}

	t.Run("subagent in the worktree, connection on the main checkout", func(t *testing.T) {
		c := newAgentLSPStateConn(t, onMain, inWorktree, onMain, inWorktree)
		assertLines(t, "subagent", c.orient(t, c.subC, "full"), worktreeServer)
		assertLines(t, "coordinator", c.orient(t, c.coord, "full"), mainServer)
		if brief := c.orient(t, c.subC, "brief"); !strings.Contains(brief, runsLine(seededGoWork)) {
			t.Errorf("subagent brief packet lacks the GOWORK=off line:\n%s", brief)
		}
	})
	t.Run("subagent on the main checkout, connection in the worktree", func(t *testing.T) {
		c := newAgentLSPStateConn(t, inWorktree, onMain, onMain, inWorktree)
		assertLines(t, "subagent", c.orient(t, c.subC, "full"), mainServer)
		assertLines(t, "coordinator", c.orient(t, c.coord, "full"), worktreeServer)
		if brief := c.orient(t, c.subC, "brief"); goWorkLineAny(brief) {
			t.Errorf("subagent on the main checkout told it runs with GOWORK=off:\n%s", brief)
		}
	})
}

// TestSessionStartLSPStateWiredPerAgent pins the registration the tests above
// copy: session_start's LSP-state accessors must be the per-workspace ones,
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
		"WithLSPServer(s.lspServerIn)",
		"WithLSPWarmup(s.lspWarmingIn)",
		"WithLSPDiagMode(s.lspDiagModeIn)",
		"WithLSPGoWorkOff(s.lspGoWorkOffIn)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("session_start is not registered with %s", want)
		}
	}
}
