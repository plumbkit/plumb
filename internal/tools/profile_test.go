package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		NewUndoEdit(WriteDeps{}),
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
		NewTasks(WriteDeps{}, nil),
	}
}

// nonLeanToolSet instantiates every registered tool NOT in the lean set, again
// with nil/zero dependencies (only the metadata methods are called). Together
// with leanToolSet it mirrors the full tool registration — TestFullToolSet_Count
// derives the expected count from registerAllTools itself rather than a
// hardcoded literal — so the budget test can measure the real lean-vs-full
// payload reduction.
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
		NewFileStatus(nil),
		NewSearchInFiles(nil, nil, nil, 0),
		NewFindFiles(nil),
		NewCopyFile(WriteDeps{}),
		NewGitInit(WriteDeps{}),
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
		NewMoveSymbol(nil, 0),
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
		NewRunCommand(nil),
		NewExecuteShellCommand(nil),
		NewShareIntent(CollabDeps{}),
		NewLeaveNote(CollabDeps{}),
		NewShareFindings(ShareFindingsDeps{}),
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

// registeredToolCount scans internal/cli/conn_register.go for
// "srv.Register(tools.New...)" lines and returns how many tools it registers —
// the actual registration count, derived from source rather than a hardcoded
// literal. This mirrors the source-scan technique
// TestToolProfileClassification (internal/cli/conn_profile_test.go) uses to
// keep the lean classification honest; the logic is duplicated here (rather
// than shared) because internal/tools cannot import internal/cli — cli sits
// above tools in the layered architecture, the same constraint that made
// levenshtein get duplicated between internal/mcp and internal/tools.
func registeredToolCount(t *testing.T) int {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "cli", "conn_register.go"))
	if err != nil {
		t.Fatalf("reading ../cli/conn_register.go: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "srv.Register(tools.New") {
			n++
		}
	}
	return n
}

// TestFullToolSet_Count guards that lean + non-lean is the whole registration.
// The expected count comes from registeredToolCount (registerAllTools itself)
// rather than a hardcoded literal, so adding a tool to conn_register.go without
// updating leanToolSet/nonLeanToolSet fails this test loudly instead of
// silently skewing TestLeanProfileBudget's ratio. The cli source-guard
// (TestToolProfileClassification) ties LeanTools to the actual
// registerAllTools; this ties the two test sets in this file to the same
// source of truth.
func TestFullToolSet_Count(t *testing.T) {
	registered := registeredToolCount(t)
	full := len(leanToolSet()) + len(nonLeanToolSet())
	if full != registered {
		t.Errorf("lean(%d) + non-lean(%d) = %d tools, want %d (registerAllTools registration count) — update the sets",
			len(leanToolSet()), len(nonLeanToolSet()), full, registered)
	}
}

// TestLeanProfileBudget asserts the lean profile's payload is a substantial
// reduction over the full list — that IS the feature. The lean set still
// contains the heavyweight write tools, so the win is hiding the non-lean
// commodity tools (plus the description diet), not an absolute floor. The
// ratio cap guards the reduction without pinning brittle absolute byte counts.
func TestLeanProfileBudget(t *testing.T) {
	lean := payloadBytes(t, leanToolSet())
	full := payloadBytes(t, append(leanToolSet(), nonLeanToolSet()...))
	// The lean set legitimately grew as heavyweight write tools gained steering
	// (edit_file anchor-bounded mode, rename_symbol/find_replace unified diffs),
	// so the cap has a little headroom over half the full tools/list payload.
	const maxRatio = 0.52
	ratio := float64(lean) / float64(full)
	t.Logf("tools/list payload: lean=%d B, full=%d B (lean is %.0f%% of full)", lean, full, ratio*100)
	if ratio > maxRatio {
		t.Errorf("lean payload is %.0f%% of full (%d/%d B), over the %.0f%% cap — the profile should hide more or descriptions grew",
			ratio*100, lean, full, maxRatio*100)
	}
}

// TestLeanProfileNote_Budget guards the lean ProfileNote sentence — including
// the folded-in reason clause — against runaway growth: it must stay well
// under the session_start orientation budget even at a 3-digit hidden count
// and the longest known reason string ("unverified-deferred-discovery", 29
// bytes — two bytes longer than "verified-deferred-discovery", 27 bytes).
func TestLeanProfileNote_Budget(t *testing.T) {
	const budget = 256
	for _, hidden := range []int{0, 9, 34, 999} {
		if got := len(ProfileNote("lean", hidden, "unverified-deferred-discovery")); got > budget {
			t.Errorf("ProfileNote(lean, %d, ...) = %d bytes, over budget %d", hidden, got, budget)
		}
	}
}

// TestProfileNote_FullReasonAndUnwired covers the full-profile line (one
// compact sentence naming the reason) and the legacy silent case: a "full"
// profile with an empty reason (the state an unwired accessor's default
// resolves to) must produce no output at all.
func TestProfileNote_FullReasonAndUnwired(t *testing.T) {
	if got := ProfileNote("full", 0, "schema-discovery-only-client"); !strings.Contains(got, "Tool profile: full (reason: schema-discovery-only-client)") {
		t.Errorf("ProfileNote(full, 0, reason) = %q, want the reason line", got)
	}
	if got := ProfileNote("full", 0, ""); got != "" {
		t.Errorf("ProfileNote(full, 0, \"\") = %q, want empty (legacy/unwired silence)", got)
	}
}

// TestBootstrapToolsAreLean asserts the advertised sets stay supersets: every
// bootstrap tool must also be a lean tool, so a lean-profile connection never
// loses the always-visible orientation surface.
func TestBootstrapToolsAreLean(t *testing.T) {
	for name := range BootstrapTools {
		if !IsLean(name) {
			t.Errorf("bootstrap tool %q is not in LeanTools — the lean set must stay a superset of the bootstrap set", name)
		}
	}
}

// TestBootstrapToolsExactSet pins BootstrapTools to exactly the four
// orientation tools, so accidental growth (or shrinkage) is a reviewable
// event rather than a silent drift.
func TestBootstrapToolsExactSet(t *testing.T) {
	want := map[string]bool{
		"session_start": true,
		"git":           true,
		"read_file":     true,
		"edit_file":     true,
	}
	if len(BootstrapTools) != len(want) {
		t.Fatalf("BootstrapTools has %d entries, want exactly %d", len(BootstrapTools), len(want))
	}
	for name := range want {
		if !IsBootstrap(name) {
			t.Errorf("BootstrapTools is missing %q", name)
		}
	}
	for name := range BootstrapTools {
		if !want[name] {
			t.Errorf("BootstrapTools has unexpected member %q", name)
		}
	}
}
