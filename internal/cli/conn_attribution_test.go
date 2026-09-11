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
