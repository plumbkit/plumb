package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// session_start_detect_test.go covers the two walks behind the identity
// section's `Scale:` line and the "Recently modified files" list. Neither had a
// test before, and both reported a workspace's build output as its size: one
// user's 8 340-file census was 8 110 gitignored files and 212 tracked ones.

// writeTree materialises a map of workspace-relative paths to contents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestCountWorkspaceFiles_ExcludesGitignoredTree(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		".gitignore":          "generated/\n*.pb.go\n",
		"main.go":             "package main\n",
		"internal/a.go":       "package a\n",
		"generated/big.go":    "package generated\n",
		"generated/deep/x.go": "package deep\n",
		"api/wire.pb.go":      "package api\n",
		"README.md":           "docs\n",
	})

	total, langCount, truncated := countWorkspaceFiles(ws, []string{".go"})
	if truncated {
		t.Fatal("small tree reported as truncated")
	}
	// Counted: .gitignore, main.go, internal/a.go, README.md.
	// Excluded: the whole generated/ tree and api/wire.pb.go.
	if total != 4 {
		t.Errorf("total = %d, want 4 (gitignored generated/ and *.pb.go excluded)", total)
	}
	if langCount != 2 {
		t.Errorf("langCount = %d, want 2 (main.go, internal/a.go)", langCount)
	}
}

func TestCountWorkspaceFiles_NestedGitignoreHonoured(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"main.go":                "package main\n",
		"web/.gitignore":         "node_out/\n",
		"web/app.go":             "package web\n",
		"web/node_out/bundle.go": "package bundle\n",
		"web/node_out/b/deep.go": "package deep\n",
		"other/node_out/keep.go": "package keep\n",
	})

	total, langCount, _ := countWorkspaceFiles(ws, []string{".go"})
	// web/.gitignore only speaks for web/: other/node_out/keep.go survives.
	// Counted: main.go, web/.gitignore, web/app.go, other/node_out/keep.go.
	if total != 4 {
		t.Errorf("total = %d, want 4 (nested .gitignore prunes web/node_out only)", total)
	}
	if langCount != 3 {
		t.Errorf("langCount = %d, want 3", langCount)
	}
}

// TestCountWorkspaceFiles_HardcodedFloorWithoutGitignore pins the rule that
// gitignore is ADDITIVE: a workspace with no ignore file at all must still
// prune the hardcoded set and hidden directories, exactly as before.
func TestCountWorkspaceFiles_HardcodedFloorWithoutGitignore(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"main.go":                   "package main\n",
		"node_modules/pkg/index.go": "package pkg\n",
		"vendor/x/y.go":             "package y\n",
		"dist/out.go":               "package dist\n",
		"build/out.go":              "package build\n",
		".git/objects/ab/cd":        "blob\n",
		".venv/lib/site.go":         "package site\n",
	})
	if _, err := os.Stat(filepath.Join(ws, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("fixture must have no .gitignore: %v", err)
	}

	total, langCount, _ := countWorkspaceFiles(ws, []string{".go"})
	if total != 1 || langCount != 1 {
		t.Errorf("total=%d langCount=%d, want 1/1 — only main.go survives the hardcoded floor", total, langCount)
	}
}

// TestCountWorkspaceFiles_CommittedVendorTreeStillPruned is the other half of
// "additive": vendor/ is pruned because the floor says so, not because git
// ignores it. A repository that TRACKS its vendor tree must still not have it
// counted as workspace scale.
func TestCountWorkspaceFiles_CommittedVendorTreeStillPruned(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		".gitignore":      "# nothing ignored at all\n",
		"main.go":         "package main\n",
		"vendor/dep/d.go": "package dep\n",
	})
	total, _, _ := countWorkspaceFiles(ws, []string{".go"})
	if total != 2 {
		t.Errorf("total = %d, want 2 (.gitignore + main.go; committed vendor/ still pruned)", total)
	}
}

// TestCensusWalk_StopsAtCap proves the walk stops rather than counting a whole
// monorepo. Exercised through censusWalk with a small limit so the test does
// not have to materialise scaleWalkMaxFiles files; TestRenderScale_AnnouncesCap
// covers what the caller then prints.
func TestCensusWalk_StopsAtCap(t *testing.T) {
	ws := t.TempDir()
	files := map[string]string{}
	for i := range 20 {
		files[filepath.Join("pkg", string(rune('a'+i))+".go")] = "package p\n"
	}
	writeTree(t, ws, files)

	visited := 0
	truncated := censusWalk(ws, censusSkipDirs, 5, func(string, fs.DirEntry) { visited++ })
	if !truncated {
		t.Error("truncated = false, want true — the walk passed its cap unreported")
	}
	if visited != 5 {
		t.Errorf("visited = %d, want 5 — the walk did not stop at the cap", visited)
	}

	visited = 0
	if truncated := censusWalk(ws, censusSkipDirs, 0, func(string, fs.DirEntry) { visited++ }); truncated {
		t.Error("maxFiles=0 must mean unlimited, got truncated")
	}
	if visited != 20 {
		t.Errorf("uncapped visited = %d, want 20", visited)
	}
}

// TestCensusWalk_CapBoundary pins the OFF-BY-ONE the cap check had: it fired
// after the maxFiles'th file had been visited, so a walk that saw every file
// there was and happened to stop on exactly the cap reported itself truncated —
// and renderScale then printed "~50000+ files" for a workspace of exactly
// 50 000, which is the ceiling-read-as-a-count that renderScale's doc comment
// says the marker exists to prevent, arrived at from the other side.
//
// truncated must mean "a file was left unvisited", so it is false at the cap
// and true one past it. The visited counts are asserted alongside, because a
// fix that made truncated correct by walking one file further would be
// counting a file the caller was told about as "and more".
func TestCensusWalk_CapBoundary(t *testing.T) {
	for _, tc := range []struct {
		name          string
		files, cap    int
		wantVisited   int
		wantTruncated bool
	}{
		{"exactly at the cap is a complete walk", 5, 5, 5, false},
		{"one over the cap is truncated", 6, 5, 5, true},
		{"well under the cap", 3, 5, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			files := map[string]string{}
			for i := range tc.files {
				files[filepath.Join("pkg", string(rune('a'+i))+".go")] = "package p\n"
			}
			writeTree(t, ws, files)

			visited := 0
			truncated := censusWalk(ws, censusSkipDirs, tc.cap, func(string, fs.DirEntry) { visited++ })
			if visited != tc.wantVisited {
				t.Errorf("visited = %d, want %d", visited, tc.wantVisited)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v (%d files, cap %d)",
					truncated, tc.wantTruncated, tc.files, tc.cap)
			}
		})
	}
}

func TestRenderScale_AnnouncesCap(t *testing.T) {
	if got := renderScale(342, 287, "Go", false); got != "~342 files (287 Go)" {
		t.Errorf("uncapped = %q", got)
	}
	if got := renderScale(scaleWalkMaxFiles, 40000, "Go", true); got != "~50000+ files (40000 Go)" {
		t.Errorf("capped = %q, want a + so the ceiling does not read as a count", got)
	}
	if got := renderScale(scaleWalkMaxFiles, 0, "", true); got != "~50000+ files" {
		t.Errorf("capped, no language = %q", got)
	}
}

// TestRecentlyModifiedFiles_SkipsIgnored is the list's version of the same bug:
// generated output is the newest thing in most workspaces, so before the walk
// read .gitignore the five "recently modified files" were five build artefacts.
func TestRecentlyModifiedFiles_SkipsIgnored(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		".gitignore":      "out/\n*.log\n",
		"main.go":         "package main\n",
		"out/artefact.go": "package out\n",
		"debug.log":       "noise\n",
	})
	// Make the ignored files the newest by a clear margin.
	now := time.Now()
	older := now.Add(-time.Hour)
	for rel, mod := range map[string]time.Time{
		".gitignore":      older,
		"main.go":         older,
		"out/artefact.go": now,
		"debug.log":       now,
	} {
		abs := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.Chtimes(abs, mod, mod); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}

	got := recentlyModifiedFiles(ws, 5)
	for _, unwanted := range []string{filepath.FromSlash("out/artefact.go"), "debug.log"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("recentlyModifiedFiles returned gitignored %q: %v", unwanted, got)
		}
	}
	if !slices.Contains(got, "main.go") {
		t.Errorf("main.go missing from %v", got)
	}
}

// TestRecentlyModifiedFiles_HardcodedFloor keeps the pre-existing skip set
// honest for a workspace with no ignore file.
func TestRecentlyModifiedFiles_HardcodedFloor(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, ws, map[string]string{
		"main.go":             "package main\n",
		".idea/workspace.xml": "<x/>\n",
		"node_modules/a.js":   "//\n",
	})
	got := recentlyModifiedFiles(ws, 5)
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("recentlyModifiedFiles = %v, want [main.go]", got)
	}
}
