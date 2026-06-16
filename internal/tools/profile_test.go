package tools

import (
	"encoding/json"
	"testing"
	"time"
)

// describable is the subset of the MCP Tool contract that drives tools/list. The
// budget test reads only these — never Execute — so a nil-deps construction is
// safe (Name/Description/InputSchema return constants).
type describable interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
}

// leanToolSet instantiates every lean tool with nil/zero dependencies. Only the
// three pure metadata methods are called, so the nil deps are never dereferenced.
func leanToolSet() []describable {
	return []describable{
		NewSessionStart(nil, nil, nil, nil, nil, nil),
		NewReadFile(nil),
		NewReadSymbol(nil, nil, 0, 0, nil),
		NewFileOutline(nil, nil, 0, 0),
		NewEditFile(WriteDeps{}),
		NewWriteFile(WriteDeps{}),
		NewRenameFile(WriteDeps{}),
		NewDeleteFile(WriteDeps{}),
		NewTransactionApply(WriteDeps{}),
		NewGit(WriteDeps{}, nil),
		NewDiagnosticsWithOpener(nil, nil),
		NewGetDefinition(nil, nil, 0, 0),
		NewFindReferences(nil, nil, 0, 0),
		NewRenameSymbol(nil, 0),
		NewWorkspaceSymbols(nil, nil, 0, 0, nil),
		NewTopologySearch(nil),
		NewTopologyExplore(nil),
		NewTopologyAffected(nil),
		NewSearchMemories(nil),
	}
}

// nonLeanToolSet instantiates every registered tool NOT in the lean set, again
// with nil/zero dependencies (only the metadata methods are called). Together
// with leanToolSet it is the full 53-tool registration, so the budget test can
// measure the real lean-vs-full payload reduction.
func nonLeanToolSet() []describable {
	return []describable{
		NewFindSymbol(nil, nil, 0, 0),
		NewExplainSymbol(nil, nil, 0, 0),
		NewListSymbols(nil, nil, 0, 0),
		NewCallHierarchy(nil, 0),
		NewTypeHierarchy(nil, 0),
		NewListFiles(nil),
		NewListDirectory(nil),
		NewReadMultipleFiles(),
		NewSearchInFiles(nil, nil, nil, 0),
		NewFindFiles(nil),
		NewCopyFile(WriteDeps{}),
		NewGitInit(WriteDeps{}),
		NewTasks(WriteDeps{}, nil),
		NewAgentConfig(AgentConfigDeps{}),
		NewFileDiff(),
		NewFindReplace(),
		NewVersion(),
		NewDaemonInfoFunc("", nil, "", time.Time{}),
		NewRenameSession(nil),
		NewWorkspaceSessions(nil, ""),
		NewInsertBeforeSymbol(nil, 0),
		NewInsertAfterSymbol(nil, 0),
		NewReplaceSymbolBody(nil, 0),
		NewSafeDeleteSymbol(nil, 0),
		NewListMemories(nil),
		NewReadMemory(nil),
		NewWriteMemory(nil),
		NewDeleteMemory(nil),
		NewRelevantMemories(nil),
		NewTopologyStatus(nil, nil),
		NewTopologyImpact(nil),
		NewTopologyRoutes(nil),
		NewStructuralQuery(nil, nil),
		NewWorkspaceSearch(nil, nil),
	}
}

// toolDef mirrors the JSON shape handleToolsList emits per tool, so the measured
// bytes match the real tools/list payload.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// payloadBytes marshals tools the way handleToolsList does and returns the byte
// count of the advertised tools/list payload.
func payloadBytes(t *testing.T, set []describable) int {
	t.Helper()
	defs := make([]toolDef, 0, len(set))
	for _, tl := range set {
		defs = append(defs, toolDef{Name: tl.Name(), Description: tl.Description(), InputSchema: tl.InputSchema()})
	}
	b, err := json.Marshal(map[string]any{"tools": defs})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return len(b)
}

func TestLeanToolSet_MatchesLeanTools(t *testing.T) {
	set := leanToolSet()
	if len(set) != len(LeanTools) {
		t.Fatalf("leanToolSet has %d tools, LeanTools has %d — keep them in lockstep", len(set), len(LeanTools))
	}
	for _, tl := range set {
		if !IsLean(tl.Name()) {
			t.Errorf("leanToolSet includes %q which is not in LeanTools", tl.Name())
		}
	}
}

// TestFullToolSet_Count guards that lean + non-lean is the whole registration.
// The cli source-guard (TestToolProfileClassification) ties LeanTools to the
// actual registerAllTools; this ties the two test sets to the documented count.
func TestFullToolSet_Count(t *testing.T) {
	const registered = 53
	full := len(leanToolSet()) + len(nonLeanToolSet())
	if full != registered {
		t.Errorf("lean(%d) + non-lean(%d) = %d tools, want %d (AGENTS.md tool count) — update the sets",
			len(leanToolSet()), len(nonLeanToolSet()), full, registered)
	}
}

// TestLeanProfileBudget asserts the lean profile's payload is a substantial
// reduction over the full list — that IS the feature. The lean set still
// contains the heavyweight write tools, so the win is hiding the ~34 commodity
// tools (plus the description diet), not an absolute floor. The ratio cap guards
// the reduction without pinning brittle absolute byte counts.
func TestLeanProfileBudget(t *testing.T) {
	lean := payloadBytes(t, leanToolSet())
	full := payloadBytes(t, append(leanToolSet(), nonLeanToolSet()...))
	const maxRatio = 0.50 // lean must stay under half the full tools/list payload
	ratio := float64(lean) / float64(full)
	t.Logf("tools/list payload: lean=%d B, full=%d B (lean is %.0f%% of full)", lean, full, ratio*100)
	if ratio > maxRatio {
		t.Errorf("lean payload is %.0f%% of full (%d/%d B), over the %.0f%% cap — the profile should hide more or descriptions grew",
			ratio*100, lean, full, maxRatio*100)
	}
}

func TestLeanProfileNote_Budget(t *testing.T) {
	const budget = 256
	for _, hidden := range []int{0, 9, 34, 999} {
		if got := len(LeanProfileNote(hidden)); got > budget {
			t.Errorf("LeanProfileNote(%d) = %d bytes, over budget %d", hidden, got, budget)
		}
	}
}
