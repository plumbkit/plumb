package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
)

// renderMemoriesSection writes the session_start memories block for ws with no
// recently-modified files and returns the rendered text.
func renderMemoriesSection(t *testing.T, ws string) string {
	t.Helper()
	var sb strings.Builder
	writeSessionMemories(&sb, ws, nil)
	return sb.String()
}

func writeUserMemories(t *testing.T, ws string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := memory.Write(ws, n, "body of "+n, "notes about "+n); err != nil {
			t.Fatal(err)
		}
	}
}

func writeGeneratedMemories(t *testing.T, ws string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := memory.WriteGenerated(nil, ws, n, "session summary", "generated body", memory.Provenance{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWriteSessionMemories_MixedTiers: user-authored memories are listed,
// generated ones collapse to a single count line under the split header.
func TestWriteSessionMemories_MixedTiers(t *testing.T) {
	ws := t.TempDir()
	writeUserMemories(t, ws, "auth-design", "test-conventions")
	writeGeneratedMemories(t, ws, "episodic-20260101-alpha", "episodic-20260102-beta", "finding-20260103-gamma")

	out := renderMemoriesSection(t, ws)
	if !strings.Contains(out, "## Memories (5: 2 user, 3 generated)") {
		t.Errorf("expected the split header, got:\n%s", out)
	}
	if !strings.Contains(out, "- **auth-design**") || !strings.Contains(out, "- **test-conventions**") {
		t.Errorf("user-authored memories should be listed:\n%s", out)
	}
	if strings.Contains(out, "episodic-20260101-alpha") || strings.Contains(out, "finding-20260103-gamma") {
		t.Errorf("generated memories must not be enumerated:\n%s", out)
	}
	if !strings.Contains(out, "3 generated memories") || !strings.Contains(out, "search_memories") {
		t.Errorf("expected the generated-count line with the search_memories pointer:\n%s", out)
	}
	if !strings.Contains(out, "Use read_memory to load any of these.") {
		t.Errorf("expected the read_memory trailer:\n%s", out)
	}
}

// TestWriteSessionMemories_UserCap: more than maxListedUserMemories
// user-authored memories are capped with a browse pointer.
func TestWriteSessionMemories_UserCap(t *testing.T) {
	ws := t.TempDir()
	for i := range maxListedUserMemories + 2 {
		writeUserMemories(t, ws, fmt.Sprintf("note-%02d", i))
	}

	out := renderMemoriesSection(t, ws)
	if !strings.Contains(out, fmt.Sprintf("## Memories (%d)", maxListedUserMemories+2)) {
		t.Errorf("all-user workspace should keep the simple header:\n%s", out)
	}
	if got := strings.Count(out, "- **"); got != maxListedUserMemories {
		t.Errorf("expected %d listed memories, got %d:\n%s", maxListedUserMemories, got, out)
	}
	if !strings.Contains(out, "…and 2 more — use list_memories to browse all.") {
		t.Errorf("expected the over-cap browse pointer:\n%s", out)
	}
}

// TestWriteSessionMemories_AllGenerated: a workspace holding only generated
// memories renders the header and count line with no enumeration.
func TestWriteSessionMemories_AllGenerated(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedMemories(t, ws, "episodic-20260101-alpha", "episodic-20260102-beta")

	out := renderMemoriesSection(t, ws)
	if !strings.Contains(out, "## Memories (2: 0 user, 2 generated)") {
		t.Errorf("expected the split header, got:\n%s", out)
	}
	if strings.Contains(out, "- **") {
		t.Errorf("no memory should be enumerated on an all-generated workspace:\n%s", out)
	}
	if !strings.Contains(out, "2 generated memories") {
		t.Errorf("expected the generated-count line:\n%s", out)
	}
}

// TestWriteSessionMemories_SingularGenerated: one generated memory reads as
// "1 generated memory", not "memories".
func TestWriteSessionMemories_SingularGenerated(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedMemories(t, ws, "episodic-20260101-alpha")

	out := renderMemoriesSection(t, ws)
	if !strings.Contains(out, "1 generated memory (") {
		t.Errorf("expected singular phrasing for one generated memory:\n%s", out)
	}
}

// TestWriteSessionMemories_Empty: a workspace with no memories keeps the
// existing "None yet" guidance.
func TestWriteSessionMemories_Empty(t *testing.T) {
	out := renderMemoriesSection(t, t.TempDir())
	if !strings.Contains(out, "## Memories\n\nNone yet. Use write_memory to save project notes.") {
		t.Errorf("expected the unchanged empty-case block, got:\n%s", out)
	}
}

// TestWriteSessionMemories_UserOnly: a user-only workspace under the cap keeps
// the simple header and lists everything, with no generated line or pointer.
func TestWriteSessionMemories_UserOnly(t *testing.T) {
	ws := t.TempDir()
	writeUserMemories(t, ws, "auth-design", "test-conventions")

	out := renderMemoriesSection(t, ws)
	if !strings.Contains(out, "## Memories (2)\n") {
		t.Errorf("expected the simple header on a clean workspace:\n%s", out)
	}
	if strings.Contains(out, "user,") || strings.Contains(out, "generated") {
		t.Errorf("no tier split should appear without generated memories:\n%s", out)
	}
	if got := strings.Count(out, "- **"); got != 2 {
		t.Errorf("expected both memories listed, got %d:\n%s", got, out)
	}
	if strings.Contains(out, "…and") {
		t.Errorf("no browse pointer expected under the cap:\n%s", out)
	}
}
