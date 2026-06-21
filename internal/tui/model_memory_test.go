package tui

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/session"
)

func TestCollectMemoryWorkspaces(t *testing.T) {
	m := &Model{
		dashProjectFolder: "/launch",
		sessions: []session.Info{
			{Folder: "/repo"},
			{Folder: "/repo"},
			{Folder: "/other"},
			{Folder: ""}, // skipped
		},
	}

	got := m.collectMemoryWorkspaces()

	// Sorted by folder: /launch, /other, /repo.
	wantFolders := []string{"/launch", "/other", "/repo"}
	if len(got) != len(wantFolders) {
		t.Fatalf("collectMemoryWorkspaces returned %d entries, want %d: %+v", len(got), len(wantFolders), got)
	}
	for i, f := range wantFolders {
		if got[i].Folder != f {
			t.Fatalf("entry %d folder = %q, want %q", i, got[i].Folder, f)
		}
	}
	if !got[0].Launch || got[0].Sessions != 0 {
		t.Fatalf("/launch should be launch-only: Launch=%v Sessions=%d", got[0].Launch, got[0].Sessions)
	}
	if got[1].Sessions != 1 {
		t.Fatalf("/other Sessions = %d, want 1", got[1].Sessions)
	}
	if got[2].Sessions != 2 {
		t.Fatalf("/repo Sessions = %d, want 2 (deduped)", got[2].Sessions)
	}
}

func TestCollectMemoryWorkspacesLaunchCoincidesWithSession(t *testing.T) {
	m := &Model{
		dashProjectFolder: "/repo",
		sessions:          []session.Info{{Folder: "/repo"}},
	}
	got := m.collectMemoryWorkspaces()
	if len(got) != 1 {
		t.Fatalf("launch dir matching a session must collapse to one entry, got %d: %+v", len(got), got)
	}
	if !got[0].Launch || got[0].Sessions != 1 {
		t.Fatalf("entry = %+v, want Launch=true Sessions=1", got[0])
	}
}

func TestCollectMemoryWorkspacesEmpty(t *testing.T) {
	m := &Model{}
	if got := m.collectMemoryWorkspaces(); len(got) != 0 {
		t.Fatalf("no sessions and no launch dir, want empty, got %+v", got)
	}
}

func indexOfWorkspace(ws []memWorkspace, folder string) int {
	for i, w := range ws {
		if w.Folder == folder {
			return i
		}
	}
	return -1
}

func TestMemoryWorkspaceSwitchReloadsAndInvalidates(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	if err := memory.Write(wsA, "alpha", "alpha body", "alpha desc"); err != nil {
		t.Fatal(err)
	}
	if err := memory.Write(wsB, "beta", "beta body", "beta desc"); err != nil {
		t.Fatal(err)
	}
	if err := memory.Write(wsB, "gamma", "gamma body", "gamma desc"); err != nil {
		t.Fatal(err)
	}

	m := &Model{
		currentSection: 2,
		sessions:       []session.Info{{ID: "1", Folder: wsA}, {ID: "2", Folder: wsB}},
	}
	m.refreshMemories()

	idxB := indexOfWorkspace(m.memoryWorkspaces, wsB)
	idxA := indexOfWorkspace(m.memoryWorkspaces, wsA)
	if idxA < 0 || idxB < 0 {
		t.Fatalf("both workspaces should be listed: %+v", m.memoryWorkspaces)
	}

	// Select workspace B — its two memories load.
	m.selectWorkspace(idxB)
	if m.memoryFolder != wsB {
		t.Fatalf("memoryFolder = %q, want %q", m.memoryFolder, wsB)
	}
	if len(m.memories) != 2 {
		t.Fatalf("wsB memories = %d, want 2: %+v", len(m.memories), m.memories)
	}

	// Move the cursor and prime the body cache, then switch to A.
	m.memoryCursor = 1
	m.memoryBodyCache = "stale"
	m.memoryBodyCacheName = "gamma"

	m.selectWorkspace(idxA)
	if m.memoryFolder != wsA {
		t.Fatalf("after switch memoryFolder = %q, want %q", m.memoryFolder, wsA)
	}
	if m.memoryCursor != 0 {
		t.Fatalf("switching workspace must reset memoryCursor, got %d", m.memoryCursor)
	}
	// The stale wsB entry (gamma) must be gone, and the cache re-primed off the
	// render path for the new selection (alpha) so the body shows without a
	// disk read on the next frame.
	if m.memoryBodyCacheName != "alpha" || m.memoryBodyCache != "alpha body" {
		t.Fatalf("switching workspace must re-prime the body cache for the new selection, got %q/%q", m.memoryBodyCacheName, m.memoryBodyCache)
	}
	if len(m.memories) != 1 || m.memories[0].Name != "alpha" {
		t.Fatalf("wsA memories = %+v, want [alpha]", m.memories)
	}
}

// TestPopulateMemoryBodyOffRenderPath guards the #59 fix: the selected
// memory's body must be filled into the cache by the pointer-receiver
// populate step (off the render path), cleared on navigation, and re-filled
// for the new selection — never read from disk inside the value-copy render
// chain.
func TestPopulateMemoryBodyOffRenderPath(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "alpha", "alpha body", "alpha desc"); err != nil {
		t.Fatal(err)
	}
	if err := memory.Write(ws, "beta", "beta body", "beta desc"); err != nil {
		t.Fatal(err)
	}

	m := &Model{
		currentSection: 2,
		sessions:       []session.Info{{ID: "1", Folder: ws}},
	}
	m.refreshMemories() // primes the cache for the first selection

	// Memories sort by name: alpha at cursor 0.
	if m.memoryBodyCacheName != "alpha" || m.memoryBodyCache != "alpha body" {
		t.Fatalf("refresh should prime the cache for the selection, got %q/%q", m.memoryBodyCacheName, m.memoryBodyCache)
	}
	// The render-path reader serves the populated cache without a disk read.
	if got := m.currentMemoryBody(); got != "alpha body" {
		t.Fatalf("currentMemoryBody = %q, want %q", got, "alpha body")
	}

	// Navigate: clear the cache (as the key handlers do), then the off-render
	// populate step refills it for the new selection.
	m.memoryCursor = 1
	m.memoryBodyCache = ""
	m.memoryBodyCacheName = ""
	if got := m.currentMemoryBody(); got != "" {
		t.Fatalf("an invalidated cache must read empty on the render path, got %q", got)
	}
	m.populateMemoryBody()
	if m.memoryBodyCacheName != "beta" || m.memoryBodyCache != "beta body" {
		t.Fatalf("populate should refill for the new selection, got %q/%q", m.memoryBodyCacheName, m.memoryBodyCache)
	}

	// Outside the Memory section, populate is a no-op (no disk read for a
	// hidden panel).
	m.currentSection = 0
	m.memoryBodyCache = ""
	m.memoryBodyCacheName = ""
	m.populateMemoryBody()
	if m.memoryBodyCacheName != "" {
		t.Fatalf("populate must do nothing outside the Memory section, got %q", m.memoryBodyCacheName)
	}
}

func TestRenderMemorySectionThreeColumns(t *testing.T) {
	RebuildStyles()
	wsA := t.TempDir()
	if err := memory.Write(wsA, "alpha", "alpha body", "alpha desc"); err != nil {
		t.Fatal(err)
	}
	m := Model{
		ready:          true,
		currentSection: 2,
		focusPanel:     focusWorkspaces,
		width:          160,
		height:         24,
		leftWidth:      defaultLeftWidth,
		scrollBounds:   &scrollBounds{},
		sessions:       []session.Info{{ID: "1", Folder: wsA}},
	}
	m.refreshMemories()

	lines := strings.Split(ansiStripForTest(m.render()), "\n")

	var top, bottom string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "╭"):
			top = l
		case strings.HasPrefix(l, "╰"):
			bottom = l
		}
	}
	if got := strings.Count(top, "┬"); got != 2 {
		t.Fatalf("top border should have 2 column junctions, got %d:\n%s", got, top)
	}
	if got := strings.Count(bottom, "┴"); got != 2 {
		t.Fatalf("bottom border should have 2 column junctions, got %d:\n%s", got, bottom)
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Workspaces (1)") {
		t.Fatalf("render missing Workspaces header:\n%s", joined)
	}
	if !strings.Contains(joined, "Memories (1)") {
		t.Fatalf("render missing Memories header:\n%s", joined)
	}
	if !strings.Contains(joined, "Memory Detail") {
		t.Fatalf("render missing Memory Detail header:\n%s", joined)
	}
}

// TestMemorySectionRowWidthInvariant guards against the three-pane assembler
// ever producing a body row wider (or narrower) than the terminal — the visual
// symptom would be a scrollbar character spilling outside the right border. It
// exercises overflowing memories AND detail across several scroll positions.
func TestMemorySectionRowWidthInvariant(t *testing.T) {
	RebuildStyles()
	ws := t.TempDir()
	body := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 60)
	for i := range 12 {
		if err := memory.Write(ws, fmt.Sprintf("mem-%02d", i), body, fmt.Sprintf("description number %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	m := Model{
		ready:          true,
		currentSection: 2,
		focusPanel:     focusDetails,
		width:          120,
		height:         20,
		leftWidth:      defaultLeftWidth,
		scrollBounds:   &scrollBounds{},
		sessions:       []session.Info{{ID: "1", Folder: ws}},
	}
	m.refreshMemories()

	for _, rs := range []int{0, 3, 999} {
		for _, ls := range []int{0, 5, 999} {
			m.rightScroll, m.leftScroll = rs, ls
			assertMemoryRowWidths(t, m, rs, ls)
		}
	}
}

func TestMemoryResizeFocusedColumn(t *testing.T) {
	m := Model{currentSection: 2, width: 160, leftWidth: defaultLeftWidth}

	baseWs, baseMem, _ := m.memoryColumnWidths()

	// Focus the Workspaces pane: [/] resize the 1st column, not the 2nd.
	m.focusPanel = focusWorkspaces
	m.resizeFocusedColumn(2)
	gotWs, gotMem, _ := m.memoryColumnWidths()
	if gotWs != baseWs+2 {
		t.Fatalf("workspaces width = %d, want %d (resized 1st column)", gotWs, baseWs+2)
	}
	if gotMem != baseMem {
		t.Fatalf("memories width = %d, want unchanged %d", gotMem, baseMem)
	}

	// Focus the Memories pane: [/] resize the 2nd column.
	m.focusPanel = focusSessions
	m.resizeFocusedColumn(-2)
	gotWs2, gotMem2, _ := m.memoryColumnWidths()
	if gotMem2 != baseMem-2 {
		t.Fatalf("memories width = %d, want %d (resized 2nd column)", gotMem2, baseMem-2)
	}
	if gotWs2 != baseWs+2 {
		t.Fatalf("workspaces width = %d, want it to stay at %d", gotWs2, baseWs+2)
	}

	// Detail focus also resizes the Memories column (Detail is the remainder).
	m.focusPanel = focusDetails
	m.resizeFocusedColumn(2)
	if _, gotMem3, _ := m.memoryColumnWidths(); gotMem3 != baseMem {
		t.Fatalf("memories width = %d, want %d (Detail focus resizes Memories)", gotMem3, baseMem)
	}
}

func assertMemoryRowWidths(t *testing.T, m Model, rs, ls int) {
	t.Helper()
	bodyHeight := max(m.height-6, 1)
	lines := strings.Split(m.render(), "\n")
	// Layout: 3 header rows, top border, bodyHeight body rows, bottom border.
	for i := 4; i < 4+bodyHeight && i < len(lines); i++ {
		plain := ansiStripForTest(lines[i])
		if w := utf8.RuneCountInString(plain); w != m.width {
			t.Fatalf("body row %d width = %d, want %d (rightScroll=%d leftScroll=%d):\n%q",
				i, w, m.width, rs, ls, plain)
		}
	}
}

func TestMemoryFilterNarrowsList(t *testing.T) {
	m := Model{currentSection: 2}
	m.memories = []memory.Memory{
		{Name: "alpha", Description: "first note"},
		{Name: "beta", Description: "second note"},
		{Name: "gamma", Description: "ALSO second"},
	}
	if got := len(m.filteredMemories()); got != 3 {
		t.Fatalf("empty filter should return all memories, got %d", got)
	}
	m.memoryFilter = "second"
	got := m.filteredMemories()
	if len(got) != 2 || got[0].Name != "beta" || got[1].Name != "gamma" {
		t.Fatalf("filter %q = %+v, want [beta gamma]", m.memoryFilter, got)
	}
	m.memoryFilter = "ALPHA"
	got = m.filteredMemories()
	if len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("name match must be case-insensitive, got %+v", got)
	}
	m.memoryFilter = "no-such-memory"
	if got := m.filteredMemories(); len(got) != 0 {
		t.Fatalf("want no matches, got %+v", got)
	}
}

func TestMemoryFilterKeyLifecycle(t *testing.T) {
	m := Model{currentSection: 2, focusPanel: focusSessions}
	m.memories = []memory.Memory{{Name: "alpha"}, {Name: "beta"}}

	m = m.handleMainKeySimple("f")
	if !m.memoryFilterActive {
		t.Fatal("'f' should activate the filter")
	}

	m.memoryCursor = 1
	m, handled := m.handleMemoryFilterKey("b")
	if !handled || m.memoryFilter != "b" || m.memoryCursor != 0 {
		t.Fatalf("typing: handled=%v filter=%q cursor=%d, want handled \"b\" 0", handled, m.memoryFilter, m.memoryCursor)
	}

	if del, _ := m.handleMemoryFilterKey("backspace"); del.memoryFilter != "" {
		t.Fatalf("backspace should delete the last rune, got %q", del.memoryFilter)
	}

	if _, handled := m.handleMemoryFilterKey("down"); handled {
		t.Fatal("navigation keys must fall through to the main handler")
	}

	m, _ = m.handleMemoryFilterKey("enter")
	if m.memoryFilterActive || m.memoryFilter != "b" {
		t.Fatalf("enter: active=%v filter=%q, want closed with the query kept", m.memoryFilterActive, m.memoryFilter)
	}

	m = m.handleMainKeySimple("esc")
	if m.memoryFilter != "" {
		t.Fatalf("esc should clear the applied filter, got %q", m.memoryFilter)
	}
}

func TestMemoryEnterTogglesListAndDetail(t *testing.T) {
	m := Model{currentSection: 2, focusPanel: focusSessions}
	m = m.mainKeyEnter()
	if m.focusPanel != focusDetails {
		t.Fatalf("enter from the list should focus the detail pane, got %v", m.focusPanel)
	}
	m = m.mainKeyEnter()
	if m.focusPanel != focusSessions {
		t.Fatalf("enter from the detail pane should focus the list, got %v", m.focusPanel)
	}
}

func TestMemoryDetailShowsDatesAndStrippedBody(t *testing.T) {
	RebuildStyles()
	ws := t.TempDir()
	if err := memory.Write(ws, "alpha", "alpha body line\n", "alpha desc"); err != nil {
		t.Fatal(err)
	}
	m := &Model{currentSection: 2, sessions: []session.Info{{ID: "1", Folder: ws}}}
	m.refreshMemories()

	plain := ansiStripForTest(strings.Join(m.memoryRightLines(60), "\n"))
	if !strings.Contains(plain, "Updated") {
		t.Errorf("detail should show the Updated date:\n%s", plain)
	}
	if strings.Contains(plain, "Created") {
		t.Errorf("a user memory without created_at should not show Created:\n%s", plain)
	}
	if strings.Contains(plain, "---") || strings.Contains(plain, "description:") {
		t.Errorf("body should not repeat the frontmatter block:\n%s", plain)
	}
	if !strings.Contains(plain, "alpha body line") {
		t.Errorf("body missing:\n%s", plain)
	}
	if strings.Contains(plain, "Origin") {
		t.Errorf("a user-authored memory must not show an Origin row:\n%s", plain)
	}
}

// TestMemoryDetailShowsOriginForGenerated: a machine-written memory's detail
// panel discloses its provenance so it is never mistaken for a hand-written
// note.
func TestMemoryDetailShowsOriginForGenerated(t *testing.T) {
	RebuildStyles()
	ws := t.TempDir()
	if err := memory.WriteGenerated(nil, ws, "episodic-demo", "session summary", "body", memory.Provenance{}); err != nil {
		t.Fatal(err)
	}
	m := &Model{currentSection: 2, sessions: []session.Info{{ID: "1", Folder: ws}}}
	m.refreshMemories()

	plain := ansiStripForTest(strings.Join(m.memoryRightLines(60), "\n"))
	if !strings.Contains(plain, "Origin") || !strings.Contains(plain, "generated") {
		t.Errorf("generated memory detail should show an Origin row with its confidence:\n%s", plain)
	}
}
