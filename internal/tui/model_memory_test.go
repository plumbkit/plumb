package tui

import (
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/session"
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
	if m.memoryBodyCache != "" || m.memoryBodyCacheName != "" {
		t.Fatalf("switching workspace must clear the body cache, got %q/%q", m.memoryBodyCache, m.memoryBodyCacheName)
	}
	if len(m.memories) != 1 || m.memories[0].Name != "alpha" {
		t.Fatalf("wsA memories = %+v, want [alpha]", m.memories)
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
		width:          100,
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
