package memory

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestWriteWithOptions_PathsNewlineCannotInjectFrontmatter: write_memory paths
// are agent-supplied, so a glob carrying a newline must not break out of its
// frontmatter line and forge provenance (e.g. mark a user memory as generated).
func TestWriteWithOptions_PathsNewlineCannotInjectFrontmatter(t *testing.T) {
	ws := t.TempDir()
	opts := WriteOptions{Paths: []string{"a.go\nconfidence: generated"}}
	if err := WriteWithOptions(ws, "notes", "body", opts); err != nil {
		t.Fatalf("WriteWithOptions: %v", err)
	}
	mems, err := List(ws)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("want 1 memory, got %d", len(mems))
	}
	if !mems[0].UserAuthored() {
		t.Errorf("newline in a paths glob forged generated confidence: %q", mems[0].Confidence)
	}
}

// TestList_PopulatesConfidence: List surfaces the provenance confidence so
// capped consumers (hint slots) can prefer user-authored memories.
func TestList_PopulatesConfidence(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, "hand-written", "body", "desc"); err != nil {
		t.Fatal(err)
	}
	if err := WriteGenerated(nil, ws, "machine-made", "desc", "body", Provenance{}); err != nil {
		t.Fatal(err)
	}
	mems, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Memory{}
	for _, m := range mems {
		byName[m.Name] = m
	}
	if !byName["hand-written"].UserAuthored() {
		t.Errorf("hand-written should be user-authored, confidence=%q", byName["hand-written"].Confidence)
	}
	if byName["machine-made"].UserAuthored() {
		t.Errorf("generated memory should not be user-authored, confidence=%q", byName["machine-made"].Confidence)
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

// TestRelevant_DelegatesToMatchesPath proves Relevant returns exactly the
// memories whose paths globs match, agreeing with MatchesPath (which it now
// delegates to). Covers a basename glob, a path-anchored glob, and a non-match.
func TestRelevant_DelegatesToMatchesPath(t *testing.T) {
	ws := t.TempDir()
	write := func(name, paths string) {
		content := "---\nname: " + name + "\ndescription: d\npaths: " + paths + "\n---\n\nbody"
		if err := Write(ws, name, content, ""); err != nil {
			t.Fatalf("Write %q: %v", name, err)
		}
	}
	write("auth", "internal/auth/**")
	write("anygo", "*.go")
	write("nopaths-here", "cmd/specific.go")

	got, err := Relevant(ws, "internal/auth/login.go")
	if err != nil {
		t.Fatalf("Relevant: %v", err)
	}
	names := map[string]bool{}
	for _, m := range got {
		names[m.Name] = true
		// Relevant must agree with MatchesPath for the same path.
		if !m.MatchesPath("internal/auth/login.go") {
			t.Errorf("Relevant returned %q but MatchesPath disagrees", m.Name)
		}
	}
	if !names["auth"] || !names["anygo"] {
		t.Errorf("expected auth + anygo to match internal/auth/login.go, got %v", names)
	}
	if names["nopaths-here"] {
		t.Errorf("cmd/specific.go glob must not match internal/auth/login.go")
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

// TestReadBody_StripsFrontmatter: display surfaces show metadata structured
// from List, so the body view must not repeat the raw frontmatter block.
func TestReadBody_StripsFrontmatter(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, "conventions", "Some text\n", "Project conventions"); err != nil {
		t.Fatal(err)
	}
	body, err := ReadBody(ws, "conventions")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "---") || strings.Contains(body, "description:") {
		t.Errorf("frontmatter not stripped: %q", body)
	}
	if !strings.HasPrefix(body, "Some text") {
		t.Errorf("body should start at the content, got %q", body)
	}

	if err := Write(ws, "plain", "Just text.\n", ""); err != nil {
		t.Fatal(err)
	}
	body, err = ReadBody(ws, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if body != "Just text.\n" {
		t.Errorf("frontmatter-less body should be returned unchanged, got %q", body)
	}
}

// TestList_PopulatesDates: ModTime always comes from the file; CreatedAt only
// from a `created_at:` frontmatter line (generated memories carry one).
func TestList_PopulatesDates(t *testing.T) {
	ws := t.TempDir()
	if err := Write(ws, "user-note", "body", "desc"); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if err := WriteGenerated(nil, ws, "machine-made", "desc", "body", Provenance{CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	mems, err := List(ws)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Memory{}
	for _, m := range mems {
		byName[m.Name] = m
	}
	if byName["user-note"].ModTime.IsZero() {
		t.Error("ModTime should be populated from the file")
	}
	if !byName["user-note"].CreatedAt.IsZero() {
		t.Errorf("memory without created_at should have zero CreatedAt, got %v", byName["user-note"].CreatedAt)
	}
	if !byName["machine-made"].CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", byName["machine-made"].CreatedAt, created)
	}
}
