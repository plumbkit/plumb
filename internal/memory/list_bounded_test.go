package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMemFile(t *testing.T, ws, name, content string) {
	t.Helper()
	if err := os.MkdirAll(Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(ws), name+".md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(mems []Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.Name)
	}
	return out
}

// With no limit ListBounded is List's frontmatter view: the same names, descriptions
// and paths, no body read (ContentSHA is the one thing it leaves out).
func TestListBounded_NoLimitMatchesListMetadata(t *testing.T) {
	ws := t.TempDir()
	writeMemFile(t, ws, "alpha", "---\nname: alpha\ndescription: first\npaths: a/*.go\n---\n\nbody a\n")
	writeMemFile(t, ws, "beta", "no frontmatter at all\n")
	want, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := ListBounded(ws, 0, nil)
	if err != nil || skipped != 0 || len(got) != len(want) || len(got) != 2 {
		t.Fatalf("ListBounded = %d memories, skipped %d, err %v; want List's %d", len(got), skipped, err, len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Name != w.Name || g.Description != w.Description || strings.Join(g.Paths, ",") != strings.Join(w.Paths, ",") || g.SizeBytes != w.SizeBytes {
			t.Errorf("memory %d = %+v, want List's %+v", i, g, w)
		}
		if g.ContentSHA != "" {
			t.Errorf("%s has a content hash, which means the whole file was read", g.Name)
		}
	}
}

// A file cap and a read budget each leave memories unread and counted, never silently
// absent. Positive control for each: the same directory with room returns them all.
func TestListBounded_TheFileCapAndTheReadBudgetAreCounted(t *testing.T) {
	ws := t.TempDir()
	for i := range 6 {
		writeMemFile(t, ws, fmt.Sprintf("m%d", i), fmt.Sprintf("---\nname: m%d\npaths: x/*.go\n---\n\nbody\n", i))
	}
	all, skipped, err := ListBounded(ws, 0, nil)
	if err != nil || len(all) != 6 || skipped != 0 {
		t.Fatalf("control: %d memories, skipped %d, err %v; want all 6", len(all), skipped, err)
	}

	got, skipped, err := ListBounded(ws, 4, nil)
	if err != nil || len(got) != 4 || skipped != 2 {
		t.Errorf("a cap of 4 gave %d memories and skipped %d (%v), want 4 and 2", len(got), skipped, err)
	}

	var spent int64
	size := int64(len("---\nname: m0\npaths: x/*.go\n---\n\nbody\n"))
	got, skipped, err = ListBounded(ws, 0, func(n int64) bool {
		if spent+n > 3*size {
			return false
		}
		spent += n
		return true
	})
	if err != nil || len(got) != 3 || skipped != 3 || spent != 3*size {
		t.Errorf("a budget of three files gave %d memories (%v), skipped %d, spent %d; want 3, 3 and %d", len(got), names(got), skipped, spent, 3*size)
	}
}

// Only the head of a file is read: a body far larger than the head costs the head, a
// frontmatter that runs past it is counted and not guessed at, and a file whose
// frontmatter fits is read whatever follows it.
func TestListBounded_ReadsOnlyTheHeadAndNeverGuessesAtALongFrontmatter(t *testing.T) {
	ws := t.TempDir()
	writeMemFile(t, ws, "short", "---\nname: short\npaths: a/*.go\n---\n\n"+strings.Repeat("body ", 5000))
	writeMemFile(t, ws, "long", "---\nname: long\nsource_symbols: ["+strings.Repeat("Symbol, ", 1000)+"Last]\n---\n\nbody\n")

	var charged []int64
	got, skipped, err := ListBounded(ws, 0, func(n int64) bool { charged = append(charged, n); return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "short" || skipped != 1 {
		t.Errorf("got %v, skipped %d; want only the memory whose frontmatter fits, and the other counted", names(got), skipped)
	}
	if len(got) == 1 && (len(got[0].Paths) != 1 || got[0].Paths[0] != "a/*.go") {
		t.Errorf("the short memory's paths = %v, want the frontmatter parsed from the head", got[0].Paths)
	}
	for _, n := range charged {
		if n > ListHeadBytes {
			t.Errorf("a file was charged %d B, over the %d B head", n, ListHeadBytes)
		}
	}
	if len(charged) != 2 {
		t.Errorf("admit was asked %d times, want once per file", len(charged))
	}
}

func TestListBounded_NoDirectoryIsNoMemories(t *testing.T) {
	if got, skipped, err := ListBounded(t.TempDir(), 5, nil); err != nil || got != nil || skipped != 0 {
		t.Errorf("ListBounded of a workspace with no memories = %v, %d, %v", got, skipped, err)
	}
}
