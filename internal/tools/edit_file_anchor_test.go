package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Happy path, include_anchors=false (default): only the text strictly between
// the anchors is replaced; the anchors themselves are left in place.
func TestEditFileAnchor_BetweenAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("BEGIN\nold body\nEND\ntail\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "BEGIN\n",
		"end_anchor":   "\nEND",
		"new_string":   "new body",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "BEGIN\nnew body\nEND\ntail\n" {
		t.Errorf("anchors should be preserved, got: %q", got)
	}
}

// Happy path, include_anchors=true: the anchors are part of the replaced span,
// so the whole inclusive region collapses to new_string.
func TestEditFileAnchor_IncludeAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("keep\nBEGIN\nbody\nEND\nkeep\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":       path,
		"start_anchor":    "BEGIN",
		"end_anchor":      "END",
		"new_string":      "REPLACED",
		"include_anchors": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep\nREPLACED\nkeep\n" {
		t.Errorf("inclusive span should be replaced, got: %q", got)
	}
}

// Empty new_string with include_anchors=false deletes only the interior,
// leaving the anchors adjacent.
func TestEditFileAnchor_EmptyNewStringDeletesInterior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("<a>middle</b>\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "<a>",
		"end_anchor":   "</b>",
		"new_string":   "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "<a></b>\n" {
		t.Errorf("interior should be deleted, anchors kept, got: %q", got)
	}
}

func TestEditFileAnchor_StartAnchorNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("BEGIN\nbody\nEND\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "NOPE",
		"end_anchor":   "END",
		"new_string":   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "start_anchor not found") {
		t.Fatalf("expected start_anchor not-found error, got: %v", err)
	}
}

func TestEditFileAnchor_AmbiguousAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("MARK\nbody\nMARK\nEND\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "MARK",
		"end_anchor":   "END",
		"new_string":   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "appears 2 times") {
		t.Fatalf("expected ambiguous start_anchor error, got: %v", err)
	}
}

func TestEditFileAnchor_EndBeforeStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("END\nbody\nBEGIN\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "BEGIN",
		"end_anchor":   "END",
		"new_string":   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "end_anchor must occur after start_anchor") {
		t.Fatalf("expected end-before-start error, got: %v", err)
	}
}

// Overlapping anchors (end_anchor begins inside start_anchor) are rejected by
// the same after-and-non-overlapping guard.
func TestEditFileAnchor_OverlappingAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("ABCDE\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "ABC",
		"end_anchor":   "CDE",
		"new_string":   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "must occur after start_anchor") {
		t.Fatalf("expected overlap rejection, got: %v", err)
	}
}

func TestEditFileAnchor_BothModesSupplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("BEGIN\nbody\nEND\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "BEGIN",
		"end_anchor":   "END",
		"new_string":   "x",
		"edits":        []map[string]string{{"old_string": "body", "new_string": "y"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected both-modes rejection, got: %v", err)
	}
}

func TestEditFileAnchor_RequiresBothAnchors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("BEGIN\nbody\nEND\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "BEGIN",
		"new_string":   "x",
	})
	if err == nil || !strings.Contains(err.Error(), "requires both start_anchor and end_anchor") {
		t.Fatalf("expected both-anchors-required error, got: %v", err)
	}
}

func TestEditFileAnchor_NeitherModeSupplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("body\n"), 0o644)

	_, err := callEditFile(t, map[string]any{"file_path": path})
	if err == nil || !strings.Contains(err.Error(), "at least one edit is required") {
		t.Fatalf("expected neither-mode rejection, got: %v", err)
	}
}

// Anchors copied verbatim from gutter-prefixed read_file output (multi-line,
// consecutive "<n>\t" prefixes) still resolve — mirroring the str_replace
// matcher's gutter forgiveness.
func TestEditFileAnchor_GutterAware(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("alpha\nbeta\ngamma\ndelta\nepsilon\n"), 0o644)

	// "1\talpha\n2\tbeta" and "4\tdelta\n5\tepsilon" as a client would paste them.
	out, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "1\talpha\n2\tbeta",
		"end_anchor":   "4\tdelta\n5\tepsilon",
		"new_string":   "\nMIDDLE\n",
	})
	if err != nil {
		t.Fatalf("guttered anchors should resolve, got: %v", err)
	}
	if !strings.Contains(out, "stripped automatically before matching") {
		t.Errorf("expected gutter-stripped advisory note, got: %q", out)
	}
	if got, _ := os.ReadFile(path); string(got) != "alpha\nbeta\nMIDDLE\ndelta\nepsilon\n" {
		t.Errorf("guttered anchor edit applied wrongly, got: %q", got)
	}
}

// CRLF tolerance: LF anchors and LF new_string match and write back into a CRLF
// file with its line endings preserved.
func TestEditFileAnchor_CRLFTolerant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("BEGIN\r\nold\r\nEND\r\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path":    path,
		"start_anchor": "BEGIN\n",
		"end_anchor":   "\nEND",
		"new_string":   "new",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "BEGIN\r\nnew\r\nEND\r\n" {
		t.Errorf("CRLF endings should be preserved, got: %q", got)
	}
}

// Regression: the existing str_replace edits mode is untouched when no anchors
// are supplied.
func TestEditFileAnchor_EditsModeUnaffected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("hello world\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "world", "new_string": "there"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "hello there\n" {
		t.Errorf("edits mode regressed, got: %q", got)
	}
}

// buildAnchorEdit unit coverage: the synthetic old_string is the full inclusive
// span (which is unique because it carries both anchors), so the downstream
// exactly-once matcher always sees a single occurrence.
func TestBuildAnchorEdit_SpanIsUnique(t *testing.T) {
	// "value" repeats outside the span, but the full inclusive span carrying the
	// unique anchors is still unique — so the downstream exactly-once matcher is
	// safe even when the interior text is not.
	content := "value\nSTART value STOP\n"
	edit, note, err := buildAnchorEdit(content, editFileArgs{
		StartAnchor: "START", EndAnchor: "STOP", NewStr: "v2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if note != "" {
		t.Errorf("no gutter note expected, got: %q", note)
	}
	if edit.OldStr != "START value STOP" {
		t.Errorf("old_string should be the inclusive span, got: %q", edit.OldStr)
	}
	if strings.Count(content, edit.OldStr) != 1 {
		t.Errorf("synthetic old_string must be unique, found %d", strings.Count(content, edit.OldStr))
	}
	if edit.NewStr != "STARTv2STOP" {
		t.Errorf("anchors should be re-attached around new_string, got: %q", edit.NewStr)
	}
}
