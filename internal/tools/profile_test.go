package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
		NewExplainSymbol(nil, nil, 0, 0),
		NewCallHierarchy(nil, 0),
		NewTypeHierarchy(nil, 0),
		NewMinimalDiffReview(nil),
		NewReadMultipleFiles(),
		NewFileStatus(nil),
		NewSearchInFiles(nil, nil, nil, 0),
		NewFindFiles(nil),
		NewCopyFile(WriteDeps{}),
		NewGitInit(WriteDeps{}),
		NewAgentConfig(AgentConfigDeps{}),
		NewFileDiff(),
		NewFindReplace(),
		NewDaemonInfoFunc(nil, nil, "", time.Time{}),
		NewRenameSession(nil),
		NewWorkspaceSessions(nil, nil),
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
		NewMutationTest(WriteDeps{}, nil),
		NewRunCommand(nil),
		NewExecuteShellCommand(nil),
		NewShareIntent(CollabDeps{}),
		NewLeaveNote(CollabDeps{}),
		NewCheckMessages(CollabDeps{}),
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

// pinnedToolSet instantiates every tool in tools.PinnedTools with nil/zero
// dependencies, mirroring leanToolSet/nonLeanToolSet — only the three pure
// metadata methods are called, so the nil deps are never dereferenced.
func pinnedToolSet() []describable {
	return []describable{
		NewSessionStart(nil, nil, nil, nil, nil, nil),
		NewReadFile(nil),
		NewReadSymbol(nil, nil, 0, 0, nil),
		NewFileOutline(nil, nil, 0, 0),
		NewEditFile(WriteDeps{}),
		NewWriteFile(WriteDeps{}),
		NewGit(WriteDeps{}, nil),
		NewDiagnosticsWithOpener(nil, nil),
		NewWorkspaceSearch(nil, nil),
		NewSearchInFiles(nil, nil, nil, 0),
		NewGetDefinition(nil, nil, 0, 0),
		NewFindReferences(nil, nil, 0, 0),
		NewWorkspaceSymbols(nil, nil, 0, 0, nil),
		NewTopologySearch(nil),
		NewTopologyAffected(nil),
		NewTransactionApply(WriteDeps{}),
		NewTasks(WriteDeps{}, nil),
		NewSearchMemories(nil),
		NewLeaveNote(CollabDeps{}),
		NewCheckMessages(CollabDeps{}),
	}
}

// TestPinnedSetMatchesPinnedTools mirrors TestLeanToolSet_MatchesLeanTools:
// pinnedToolSet and tools.PinnedTools must stay in lockstep, or the budget
// test below would be silently measuring the wrong set.
func TestPinnedSetMatchesPinnedTools(t *testing.T) {
	set := pinnedToolSet()
	if len(set) != len(PinnedTools) {
		t.Fatalf("pinnedToolSet has %d tools, PinnedTools has %d — keep them in lockstep", len(set), len(PinnedTools))
	}
	for _, tl := range set {
		if !IsPinned(tl.Name()) {
			t.Errorf("pinnedToolSet includes %q which is not in PinnedTools", tl.Name())
		}
	}
}

// maxPinnedBytes bounds the serialized payload of the tools Claude Code pins
// into context on every connection (name + description + schema JSON, the same
// shape handleToolsList emits).
//
// PLAN-355 originally proposed 15,000 — measurement shows that is not
// reachable for this pin set without cutting into schema JSON (out of this
// card's scope, which is descriptions only): the four BootstrapTools alone
// (session_start, read_file, edit_file, git), which must stay pinned for their
// own stated reasons regardless of PinnedTools' contents, already total 16,663
// bytes — over 15,000 before a single other tool is added. Schema JSON, not
// description text, dominates: edit_file's and git's schemas alone are 5,257
// and 3,157 bytes respectively, driven by per-parameter documentation the
// description-budget work in this card does not touch. Measured (not guessed,
// same discipline as maxDescriptionChars above): the full 20-tool
// PinnedTools payload is ~42,700 bytes today. The cap below is that
// measurement plus modest headroom — a ratchet against payload growth, not an
// aspirational target this card's description trims could ever reach alone.
const maxPinnedBytes = 45000

// TestPinnedSetBudget guards maxPinnedBytes.
func TestPinnedSetBudget(t *testing.T) {
	pinned := payloadBytes(t, pinnedToolSet())
	t.Logf("pinned tools/list payload: %d bytes (%d tools)", pinned, len(pinnedToolSet()))
	if pinned > maxPinnedBytes {
		t.Errorf("pinned payload is %d bytes, over the %d-byte budget — trim a description or evict a tool from PinnedTools",
			pinned, maxPinnedBytes)
	}
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

// maxDescriptionChars is the per-tool description ceiling. Clients truncate a
// long description before the model ever sees it and append "… [truncated]", so
// every character past the cap is authored, shipped, and then discarded — and
// the discarded part is always the TAIL: the parameter notes and caveats that
// tend to sit at the end.
//
// The client constant is not published, but it is directly measurable, and it
// was measured rather than guessed. Taking the six descriptions that arrived
// truncated in a live Claude Code session and locating the last surviving word
// of each in the source string puts the cut at exactly 2048 for all six —
// edit_file, git, move_symbol, mutation_test, leave_note and check_messages,
// whose full lengths span 2,077 to 2,909.
//
// That measurement fixes the UNIT as well as the value, which is the part worth
// writing down: the six cut points agree at 2048 in RUNES and disagree in bytes
// (2,056–2,064, the spread being each description's own em dashes and arrows).
// A byte-counting ceiling would therefore be measuring the wrong quantity, and
// would reject text the client would have delivered. Count runes.
//
// 2000 rather than 2048 is deliberate headroom: it is one client's constant, not
// a spec, and a second client may well cap lower. The cap is a ratchet — under
// it a description is known to survive.
//
// When a description outgrows this, MOVE the material, do not delete it: the
// shipped skills (internal/cli/skills/) and docs/tools.md are the places
// doctrine belongs, and unlike a description they are not silently clipped.
const maxDescriptionChars = 2000

// maxPinnedDescriptionChars is the tighter per-description budget for the
// tools that previously drifted past maxDescriptionChars and got truncated in
// a live Claude Code session: edit_file, git, move_symbol, leave_note, and
// check_messages (PLAN-323/PLAN-355). All five are now candidates for
// tools.PinnedTools — a pinned description is paid for on EVERY connection,
// not just when a client's tool search happens to page it in, so it earns a
// stricter ceiling than the general 2000-rune truncation cap: one behaviour
// sentence plus one parameter-shape sentence, with everything else moved into
// the tool's skill (internal/cli/skills/).
const maxPinnedDescriptionChars = 1200

// tightenedDescriptionTools names the five descriptions maxPinnedDescriptionChars
// applies to (see its doc comment). Kept as an explicit list, not derived from
// PinnedTools, because the tighter budget is a property of THESE descriptions'
// history of drifting past the client truncation cap — not of pin membership in
// general; a future PinnedTools addition does not automatically inherit it.
var tightenedDescriptionTools = map[string]bool{
	"edit_file":      true,
	"git":            true,
	"move_symbol":    true,
	"leave_note":     true,
	"check_messages": true,
}

// TestDescriptionRuneCeiling pins every advertised description under the
// client truncation cap, and the five tools named in tightenedDescriptionTools
// under the tighter pinned-description budget.
//
// Nothing asserted the general cap before PLAN-323, which is exactly why it
// recurred silently: six descriptions had drifted past it — including
// edit_file and git, both BootstrapTools that every agent receives on every
// task — and the only symptom was text quietly missing from the model's
// context. A build that never fails cannot tell you the tool description you
// just wrote is not being read.
func TestDescriptionRuneCeiling(t *testing.T) {
	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		n := len([]rune(tl.Description()))
		if n > maxDescriptionChars {
			t.Errorf("%s description is %d chars, over the %d-char client truncation cap by %d — a client will clip the tail before the model sees it; move the overflow into the tool's skill (internal/cli/skills/) or docs/tools.md rather than deleting it",
				tl.Name(), n, maxDescriptionChars, n-maxDescriptionChars)
		}
		if tightenedDescriptionTools[tl.Name()] && n > maxPinnedDescriptionChars {
			t.Errorf("%s description is %d chars, over the %d-char pinned-description budget by %d — move lore into the tool's skill rather than the description",
				tl.Name(), n, maxPinnedDescriptionChars, n-maxPinnedDescriptionChars)
		}
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

// TestLeanToolNames_SortedUnion pins the client-side allowlist accessor to its
// contract: the sorted, deduplicated union of LeanTools and BootstrapTools. The
// expectation is COMPUTED from the two maps, never a hardcoded count — growing
// or shrinking the lean set must change the allowlist, not fail this test.
func TestLeanToolNames_SortedUnion(t *testing.T) {
	want := map[string]bool{}
	for name := range LeanTools {
		want[name] = true
	}
	for name := range BootstrapTools {
		want[name] = true
	}

	got := LeanToolNames()
	if len(got) != len(want) {
		t.Fatalf("LeanToolNames() has %d entries, want %d (|LeanTools ∪ BootstrapTools|)", len(got), len(want))
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("LeanToolNames() is not sorted: %v", got)
	}
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Errorf("LeanToolNames() repeats %q", name)
		}
		seen[name] = true
		if !want[name] {
			t.Errorf("LeanToolNames() includes %q, which is in neither LeanTools nor BootstrapTools", name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("LeanToolNames() is missing %q", name)
		}
	}
}

// TestLeanToolNames_CoversBootstrap states the invariant the union exists for:
// a client-side allowlist must never be able to strip a bootstrap tool, since
// the client — not plumb — enforces it, and plumb's always-advertise guarantee
// cannot reach that far.
func TestLeanToolNames_CoversBootstrap(t *testing.T) {
	in := map[string]bool{}
	for _, name := range LeanToolNames() {
		in[name] = true
	}
	for name := range BootstrapTools {
		if !in[name] {
			t.Errorf("bootstrap tool %q is absent from the client-side allowlist", name)
		}
	}
}

// TestMailboxToolsArePairedAndNonLean pins the invariant the pin set exists
// for. The mailbox halves instruct the agent to call each other, so they must be
// pinned TOGETHER — an asymmetry (send reachable, receive not) is the defect
// itself. And they are deliberately NOT lean, which is precisely why the pin is
// load-bearing: without it, nothing else in the profile machinery keeps them in
// a pinning client's context.
func TestMailboxToolsArePairedAndNonLean(t *testing.T) {
	want := []string{"leave_note", "check_messages"}
	if len(MailboxTools) != len(want) {
		t.Fatalf("MailboxTools has %d entries, want exactly %d — the pair is the contract", len(MailboxTools), len(want))
	}
	for _, name := range want {
		if !IsMailbox(name) {
			t.Errorf("MailboxTools is missing %q — the halves must be pinned together", name)
		}
		if IsLean(name) {
			t.Errorf("%q is in LeanTools; MailboxTools documents itself as the non-lean pair — "+
				"reconcile the two rather than leaving them contradictory", name)
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
