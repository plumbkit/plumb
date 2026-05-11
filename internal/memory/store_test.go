package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPath_RejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../etc", "a/b", "a.b", "a b", ""} {
		if _, err := Path("/ws", bad); err == nil {
			t.Errorf("Path(%q) should be rejected", bad)
		}
	}
	got, err := Path("/ws", "auth_arch-2")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/ws", ".plumb", "memories", "auth_arch-2.md") {
		t.Errorf("unexpected path: %s", got)
	}
}

func TestWriteReadList(t *testing.T) {
	ws := t.TempDir()

	if err := Write(ws, "conventions", "Some text\n", "Project conventions"); err != nil {
		t.Fatal(err)
	}
	if err := Write(ws, "gotchas", "Watch out for X.\n", ""); err != nil {
		t.Fatal(err)
	}

	got, err := Read(ws, "conventions")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "name: conventions") || !strings.Contains(got, "description: Project conventions") {
		t.Errorf("frontmatter missing: %s", got)
	}
	if !strings.Contains(got, "Some text") {
		t.Errorf("body missing: %s", got)
	}

	got2, err := Read(ws, "gotchas")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got2, "---") {
		t.Errorf("gotchas should not have frontmatter (no description was passed): %s", got2)
	}

	mems, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("want 2 memories, got %d", len(mems))
	}
	if mems[0].Name != "conventions" || mems[0].Description != "Project conventions" {
		t.Errorf("conventions memory wrong: %+v", mems[0])
	}
	if mems[1].Name != "gotchas" || mems[1].Description != "" {
		t.Errorf("gotchas memory wrong: %+v", mems[1])
	}
}

func TestDelete(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, "tmp", "hi\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ws, "tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(ws, "tmp"); err == nil {
		t.Error("expected read to fail after delete")
	}
	if err := Delete(ws, "tmp"); err == nil {
		t.Error("expected delete of missing memory to error")
	}
}

func TestList_EmptyDirectory(t *testing.T) {
	ws := t.TempDir()
	mems, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 0 {
		t.Errorf("want empty list, got %d memories", len(mems))
	}
}

func TestWrite_OverwriteReplacesFrontmatter(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, "x", "first content\n", "first description"); err != nil {
		t.Fatal(err)
	}
	if err := Write(ws, "x", "second content\n", "second description"); err != nil {
		t.Fatal(err)
	}
	got, err := Read(ws, "x")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "---") != 2 {
		t.Errorf("expected exactly two `---` markers, got: %s", got)
	}
	if !strings.Contains(got, "second description") || strings.Contains(got, "first description") {
		t.Errorf("description not overwritten: %s", got)
	}
	if !strings.Contains(got, "second content") {
		t.Errorf("body not overwritten: %s", got)
	}
}
