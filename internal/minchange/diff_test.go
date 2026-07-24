package minchange

import "testing"

func TestParseUnifiedDiff_NewFileHunksAndLineNumbers(t *testing.T) {
	raw := `diff --git a/pkg/foo.go b/pkg/foo.go
new file mode 100644
index 0000000..1111111
--- /dev/null
+++ b/pkg/foo.go
@@ -0,0 +1,3 @@
+package pkg
+
+func Foo() {}
`
	d := ParseUnifiedDiff(raw)
	if len(d.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(d.Files))
	}
	f := d.Files[0]
	if f.Path != "pkg/foo.go" {
		t.Errorf("path = %q, want pkg/foo.go", f.Path)
	}
	if !f.IsNew {
		t.Errorf("IsNew = false, want true for a --- /dev/null file")
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	added := addedLines(f.Hunks[0])
	if len(added) != 3 {
		t.Fatalf("want 3 added lines, got %d", len(added))
	}
	// New-side numbering starts at 1 and advances per added/context line.
	if added[0].NewLineNo != 1 || added[2].NewLineNo != 3 {
		t.Errorf("line numbers = %d,%d want 1,3", added[0].NewLineNo, added[2].NewLineNo)
	}
	if added[2].Text != "func Foo() {}" {
		t.Errorf("added[2].Text = %q", added[2].Text)
	}
}

func TestParseUnifiedDiff_HunkBodyDashPrefixesStayContent(t *testing.T) {
	// A removed "-- " SQL comment renders as "--- …" in the hunk body (and an
	// added "++ …" line as "+++ …"); neither is a file header and neither may
	// truncate the hunk or corrupt the file's paths.
	raw := `diff --git a/x.sql b/x.sql
--- a/x.sql
+++ b/x.sql
@@ -1,3 +1,3 @@
 SELECT 1;
--- drop table users
+++ keep table users
 SELECT 2;
`
	d := ParseUnifiedDiff(raw)
	if len(d.Files) != 1 {
		t.Fatalf("want 1 file, got %d", len(d.Files))
	}
	f := d.Files[0]
	if f.Path != "x.sql" || f.OldPath != "x.sql" {
		t.Errorf("paths corrupted by hunk-body lines: Path=%q OldPath=%q", f.Path, f.OldPath)
	}
	if f.IsNew || f.IsDelete {
		t.Errorf("IsNew=%v IsDelete=%v, want false: hunk-body lines misread as headers", f.IsNew, f.IsDelete)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	var removed, added []Line
	for _, ln := range f.Hunks[0].Lines {
		switch ln.Kind {
		case Removed:
			removed = append(removed, ln)
		case Added:
			added = append(added, ln)
		}
	}
	if len(removed) != 1 || removed[0].Text != "-- drop table users" {
		t.Errorf("removed lines = %+v, want the SQL comment kept as hunk content", removed)
	}
	if len(added) != 1 || added[0].Text != "++ keep table users" {
		t.Errorf("added lines = %+v, want the ++ line kept as hunk content", added)
	}
}

func TestParseUnifiedDiff_ContextAdvancesNewSide(t *testing.T) {
	raw := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -10,3 +10,4 @@ func Outer() {
 	a := 1
+	b := 2
 	c := 3
`
	d := ParseUnifiedDiff(raw)
	f := d.Files[0]
	added := addedLines(f.Hunks[0])
	if len(added) != 1 {
		t.Fatalf("want 1 added line, got %d", len(added))
	}
	// Hunk starts at new line 10; one context line precedes the add → line 11.
	if added[0].NewLineNo != 11 {
		t.Errorf("added line number = %d, want 11", added[0].NewLineNo)
	}
	if f.IsNew || f.IsDelete {
		t.Errorf("modified file wrongly flagged new/delete")
	}
}

func TestParseUnifiedDiff_BinaryAndDelete(t *testing.T) {
	raw := `diff --git a/logo.png b/logo.png
index aaa..bbb 100644
Binary files a/logo.png and b/logo.png differ
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package gone
-func Gone() {}
`
	d := ParseUnifiedDiff(raw)
	if len(d.Files) != 2 {
		t.Fatalf("want 2 files, got %d", len(d.Files))
	}
	if !d.Files[0].IsBinary {
		t.Errorf("logo.png not flagged binary")
	}
	if !d.Files[1].IsDelete {
		t.Errorf("gone.go not flagged delete")
	}
	if d.Files[1].Path != "gone.go" {
		t.Errorf("delete path = %q, want gone.go", d.Files[1].Path)
	}
}

func addedLines(h Hunk) []Line {
	var out []Line
	for _, l := range h.Lines {
		if l.Kind == Added {
			out = append(out, l)
		}
	}
	return out
}
