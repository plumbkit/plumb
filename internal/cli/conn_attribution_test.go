package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/stats"
)

// TestAfterToolAttributionIsWired guards the wiring rather than the behaviour,
// the way TestDeclaredAgentChannelIsWired does for the sibling channel. The one
// line that makes per-agent attribution real — lifting the identity off the
// call's ctx — is reachable only through registerAllTools, so replacing it with
// "" left every stats row attributed to the connection again with the whole
// suite, integration included, still green (PLAN-401 review, finding B1).
func TestAfterToolAttributionIsWired(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := funcBodyOf(string(src), "func (s *connSession) registerHooks")
	if body == "" {
		t.Fatal("could not locate registerHooks in conn_register.go — was it renamed?")
	}
	if !strings.Contains(body, "srv.OnAfterTool = s.afterToolFromCtx") {
		t.Error("OnAfterTool is not registered as s.afterToolFromCtx: the recorded row would name " +
			"the connection rather than the agent that wrote (PLAN-401)")
	}
}

// TestAfterToolFromCtxRecordsTheCallersAgent drives the REAL hook function —
// not a copy of its body — and asserts the identity it lifts off the ctx
// reaches the stored row, that a call carrying none is stored blank, and that
// a single-agent connection stores nothing even when the calls are stamped.
func TestAfterToolFromCtxRecordsTheCallersAgent(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	s := newPersistSession(t, store, ss, "proxy-attrib")
	s.statsStore = newStatsStore()
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false); err != nil {
		t.Fatalf("pin: %v", err)
	}

	record := func(agent, path string) {
		args, _ := json.Marshal(map[string]any{"file_path": path, "content": "x"})
		s.afterToolFromCtx(mcp.WithLogicalAgent(context.Background(), agent),
			"write_file", args, "wrote", "", time.Millisecond, false, nil)
	}

	// While one agent holds the connection, a stamped call records no agent:
	// the session name already names the only writer.
	record("conv", root+"/solo.txt")

	// A second identity makes the connection shared; from here the id is what
	// tells the two apart.
	s.recordLogicalAgentAttach("conv")
	s.recordLogicalAgentCall("conv/agent-1")
	record("conv", root+"/parent.txt")
	record("conv/agent-1", root+"/sub.txt")
	record("", root+"/anon.txt")

	s.statsStore.Close()
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()
	rows, err := db.RecentWritesByWorkspace(root, []string{"write_file"}, 50)
	if err != nil {
		t.Fatalf("RecentWritesByWorkspace: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[fileFromArgs(t, r.InputJSON)] = r.LogicalAgent
	}
	for path, want := range map[string]string{
		"solo.txt":   "",             // recorded before the connection was shared
		"parent.txt": "conv",         // the parent, now one of several
		"sub.txt":    "conv/agent-1", // its subagent
		"anon.txt":   "",             // no identity presented, none invented
	} {
		if agent, ok := got[path]; !ok || agent != want {
			t.Errorf("row for %s recorded agent %q (present=%v), want %q", path, agent, ok, want)
		}
	}
}

// TestAttributedAgentSanitisesBeforeStoring: the id is client-supplied and
// ends up in a peer-facing feed, so the record path caps and strips it rather
// than trusting every renderer to.
func TestAttributedAgentSanitisesBeforeStoring(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil,
		config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	s.recordLogicalAgentAttach("a")
	s.recordLogicalAgentCall("b")

	if got := s.attributedAgent("x\nforged   write_file"); strings.ContainsAny(got, "\n\r") {
		t.Errorf("stored id kept a control character: %q", got)
	}
	if got := s.attributedAgent(strings.Repeat("z", 4096)); len(got) > stats.AgentIDStorageBytes {
		t.Errorf("stored id is %d bytes, over the %d-byte cap", len(got), stats.AgentIDStorageBytes)
	}
}

// funcBodyOf returns the source of the function starting with sig, up to the
// next top-level func. The sibling extractor in boundary_contract_test.go is
// hardcoded to registerAllTools; the hooks live in their own function.
func funcBodyOf(src, sig string) string {
	start := strings.Index(src, sig)
	if start < 0 {
		return ""
	}
	rest := src[start+len(sig):]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+len(sig)+end]
}

func fileFromArgs(t *testing.T, inputJSON string) string {
	t.Helper()
	var a struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal([]byte(inputJSON), &a)
	if i := strings.LastIndexByte(a.FilePath, '/'); i >= 0 {
		return a.FilePath[i+1:]
	}
	return a.FilePath
}

// TestAfterToolFilesTheRowUnderTheAgentsOwnWorkspace: on a shared connection a
// recorded call must name the workspace the call actually ran against — the
// caller's shard root — not whichever project the CONNECTION last pinned.
//
// The two are the same on a single-agent connection, so the gap only opens when
// several agents multiplex one `plumb serve` and one of them pins elsewhere. A
// git commit is the case with teeth: its arguments carry no path for
// workspaceFromArgs to re-attribute from, so the row took the connection's pin
// wholesale. The repository's own workspace_sessions feed
// (RecentWritesByWorkspace, keyed on workspace) then had no entry for the
// commit, while the ref guard — keyed on the repository — still named the
// session that made it. Three subsystems, three answers about one commit.
func TestAfterToolFilesTheRowUnderTheAgentsOwnWorkspace(t *testing.T) {
	store, ss := newOriginStore(t)
	connRoot := freshTempDir(t)
	mustGitDir(t, connRoot)
	agentRoot := freshTempDir(t)
	mustGitDir(t, agentRoot)

	s := newPersistSession(t, store, ss, "proxy-audit-ws")
	s.statsStore = newStatsStore()
	if _, err := s.repinWorkspace(context.Background(), "file://"+connRoot, "", false); err != nil {
		t.Fatalf("connection pin: %v", err)
	}

	// A second identity makes the connection shared, so declared calls route to
	// their own shards.
	s.recordLogicalAgentAttach("agent-here")
	s.recordLogicalAgentCall("agent-elsewhere")
	ctx := mcp.WithLogicalAgent(context.Background(), "agent-elsewhere")
	if moved, refused := s.repinAgent(ctx, agentRoot, "", sessionstate.PinSourceSessionStart, true); refused != nil || !moved {
		t.Fatalf("agent pin: moved=%v refused=%v", moved, refused)
	}
	if got := s.workspaceFor(ctx); got != agentRoot {
		t.Fatalf("precondition: the agent's calls resolve against %q, want %q", got, agentRoot)
	}
	if got := s.view().acquiredRoot; got != connRoot {
		t.Fatalf("precondition: the connection must still hold %q, got %q", connRoot, got)
	}

	args, err := json.Marshal(map[string]any{"subcommand": "commit", "message": "audit me"})
	if err != nil {
		t.Fatalf("marshal git args: %v", err)
	}
	s.afterToolFromCtx(ctx, "git", args, "abc1234 audit me", "", time.Millisecond, false, nil)

	s.statsStore.Close()
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()

	rows, err := db.RecentWritesByWorkspace(agentRoot, []string{"git"}, 50)
	if err != nil {
		t.Fatalf("RecentWritesByWorkspace(agentRoot): %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("the repository that was committed to has %d audit rows, want 1 — "+
			"its own feed cannot show a commit plumb itself mediated", len(rows))
	}
	stray, err := db.RecentWritesByWorkspace(connRoot, []string{"git"}, 50)
	if err != nil {
		t.Fatalf("RecentWritesByWorkspace(connRoot): %v", err)
	}
	if len(stray) != 0 {
		t.Errorf("the connection's workspace has %d audit rows for a commit made in another project, want 0", len(stray))
	}
}
