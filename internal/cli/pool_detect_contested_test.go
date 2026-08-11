package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// contestedPool builds a pool whose java and kotlin languages are both active,
// with the shipped default root markers. Both point at a binary that certainly
// exists so the result does not depend on what is installed on the test machine.
func contestedPool(t *testing.T, extra ...string) *workspacePool {
	t.Helper()
	cfg := config.Defaults()
	keep := append([]string{"java", "kotlin"}, extra...)
	for name := range cfg.LSP {
		if !contains(keep, name) {
			delete(cfg.LSP, name)
			continue
		}
		c := cfg.LSP[name]
		c.Command = "go" // always present: the toolchain running this test
		c.Enabled = true
		cfg.LSP[name] = c
	}
	return newWorkspacePool(context.Background(), cfg)
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// writeFiles creates each relative path under root with trivial content.
func writeFiles(t *testing.T, root string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		p := filepath.Join(root, filepath.FromSlash(r))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("// x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStrongLangAt_ContestedGradleRootFollowsSources is the regression test for
// the routing bug this tie-break fixes. `build.gradle.kts` is a strong root
// marker for BOTH java and kotlin and both ship enabled, so before the
// tie-break a Kotlin-only Gradle project resolved to java purely because "java"
// sorts first — starting a jdtls JVM for a project with no Java in it.
func TestStrongLangAt_ContestedGradleRootFollowsSources(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name: "kotlin-only Kotlin DSL project",
			files: []string{
				"settings.gradle.kts", "build.gradle.kts",
				"src/main/kotlin/App.kt", "src/main/kotlin/Greeter.kt",
			},
			want: "kotlin",
		},
		{
			name: "java project using the Kotlin Gradle DSL",
			files: []string{
				"settings.gradle.kts", "build.gradle.kts",
				"src/main/java/App.java", "src/main/java/Widget.java",
			},
			want: "java",
		},
		{
			name: "mixed project, Kotlin dominant",
			files: []string{
				"build.gradle.kts",
				"src/main/kotlin/A.kt", "src/main/kotlin/B.kt", "src/main/kotlin/C.kt",
				"src/main/java/One.java",
			},
			want: "kotlin",
		},
		{
			name: "mixed project, Java dominant",
			files: []string{
				"build.gradle.kts",
				"src/main/java/A.java", "src/main/java/B.java", "src/main/java/C.java",
				"src/main/kotlin/One.kt",
			},
			want: "java",
		},
		{
			name:  "no sources at all falls back to language order",
			files: []string{"build.gradle.kts"},
			want:  "java",
		},
		{
			// An exclusive marker is evidence, not a veto: pom.xml is java's
			// alone, but holding it alongside the contested build.gradle.kts
			// does not exempt java from the tie. A Gradle build that kept a
			// legacy pom.xml around should still attach the language its
			// sources are written in. Stated so the case is a decision on the
			// record rather than a side effect of how `matched` is collected.
			name: "exclusive java marker beside the contested one",
			files: []string{
				"pom.xml", "build.gradle.kts",
				"src/main/kotlin/A.kt", "src/main/kotlin/B.kt",
			},
			want: "kotlin",
		},
		{
			// The standard Gradle MULTI-project, and the shape that caught an
			// earlier version of this tie-break: one build script per module.
			// Every nested build.gradle.kts is a Kotlin file by extension, so
			// counting them handed a Java project with no Kotlin in it to
			// kotlin as soon as it had more modules than the root had sources
			// — this bug's own mirror image. Markers are ignored wherever they
			// sit, not just in the root.
			name: "java multi-project: one build script per module",
			files: []string{
				"settings.gradle.kts", "build.gradle.kts",
				"a/build.gradle.kts", "b/build.gradle.kts", "c/build.gradle.kts",
				"a/src/App.java", "b/src/Lib.java",
			},
			want: "java",
		},
		{
			// The same shape at full JVM depth: <module>/src/main/java/<pkg>.
			// Sources sit six directories down, one past what the tie-break
			// used to reach, so the scan saw nothing but build scripts.
			name: "java multi-project at full package depth",
			files: []string{
				"settings.gradle.kts", "build.gradle.kts",
				"app/build.gradle.kts", "core/build.gradle.kts",
				"app/src/main/java/com/example/App.java",
				"app/src/main/java/com/example/Widget.java",
				"core/src/main/java/com/example/Core.java",
			},
			want: "java",
		},
		{
			// The case the fix is FOR, at that same depth. It must resolve
			// kotlin because of App.kt and Greeter.kt, NOT because three
			// build scripts happen to be Kotlin by extension — which is how it
			// passed before, with the real sources never reached at all.
			name: "kotlin multi-project at full package depth",
			files: []string{
				"settings.gradle.kts", "build.gradle.kts",
				"app/build.gradle.kts",
				"app/src/main/kotlin/com/example/App.kt",
				"app/src/main/kotlin/com/example/Greeter.kt",
			},
			want: "kotlin",
		},
		{
			// Both Kotlin-DSL scripts, no sources: every marker is ignored, so
			// there is no evidence either way and the deterministic order
			// decides. Distinct from the single-script case above because it
			// exercises a marker beyond the first in a language's list —
			// settings.gradle.kts is kotlin's, and if only the first matched
			// language's markers were ignored it would vote kotlin here.
			name:  "both Kotlin DSL scripts and no sources falls back to order",
			files: []string{"settings.gradle.kts", "build.gradle.kts"},
			want:  "java",
		},
		{
			name: "equal counts fall back to language order",
			files: []string{
				"build.gradle.kts",
				"src/main/kotlin/A.kt", "src/main/java/A.java",
			},
			want: "java",
		},
		{
			name: "sources nested in package directories still counted",
			files: []string{
				"build.gradle.kts",
				"src/main/kotlin/com/example/App.kt",
				"src/main/kotlin/com/example/Greeter.kt",
			},
			want: "kotlin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, tc.files...)
			if got := contestedPool(t).strongLangAt(dir); got != tc.want {
				t.Fatalf("strongLangAt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStrongLangAt_ThirdLanguageCannotWinTheTie drives dominantAmong's
// restriction through strongLangAt rather than asserting it on the helper alone.
// The distinction is not academic: the count map covers every ACTIVE language
// under dir, not just the tied ones, so a Gradle root with a big scripts/ tree
// beside it would resolve to whatever language owns that tree — naming a server
// no marker in the directory ever asked for. TestDominantAmong_...OutsideTheTie
// pins the helper's contract; this pins that strongLangAt actually uses it, which
// the helper test cannot see (swapping dominantAmong for bestSniffedLang at the
// call site leaves it green).
func TestStrongLangAt_ThirdLanguageCannotWinTheTie(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"build.gradle.kts",
		"src/main/kotlin/A.kt", "src/main/kotlin/B.kt",
		"src/main/java/One.java",
		"scripts/a.go", "scripts/b.go", "scripts/c.go", "scripts/d.go",
		"scripts/e.go", "scripts/f.go", "scripts/g.go", "scripts/h.go")

	// go is active and owns the most files by far, but no marker in dir names
	// it, so it is not a candidate and cannot take the tie.
	if got := contestedPool(t, "go").strongLangAt(dir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin — go owns most of the tree but "+
			"claims no root marker here, so it was never part of the tie", got)
	}
}

// TestStrongLangAt_UncontestedMarkerIgnoresNeighbouringSources pins that the
// tie-break is confined to a genuinely contested directory: a marker only one
// language claims resolves to that language whatever sources happen to sit
// beside it.
//
// Named for what it actually checks. An earlier name promised the uncontested
// case "skips the sniff", and mutation testing showed that half was vacuous:
// deleting strongLangAt's len(matched)==1 early return leaves this test — and
// the whole internal/cli suite — green, because dominantAmong restricted to a
// single candidate can only return that candidate or "", and both paths end at
// the same answer. Skipping the scan is a performance property, invisible
// through the return value and so not pinnable here; see the comment on that
// return. What IS pinned, and is worth pinning, is the outcome below.
func TestStrongLangAt_UncontestedMarkerIgnoresNeighbouringSources(t *testing.T) {
	dir := t.TempDir()
	// pom.xml is java's alone. The Kotlin sources beside it must not win.
	writeFiles(t, dir, "pom.xml",
		"src/main/kotlin/A.kt", "src/main/kotlin/B.kt", "src/main/kotlin/C.kt")
	if got := contestedPool(t).strongLangAt(dir); got != "java" {
		t.Fatalf("strongLangAt = %q, want java (pom.xml is uncontested)", got)
	}

	kdir := t.TempDir()
	// settings.gradle.kts is kotlin's alone.
	writeFiles(t, kdir, "settings.gradle.kts",
		"src/main/java/A.java", "src/main/java/B.java")
	if got := contestedPool(t).strongLangAt(kdir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin (settings.gradle.kts is uncontested)", got)
	}
}

// TestDetect_KotlinGradleProjectResolvesKotlin drives the tie-break through the
// real entry point, from a source file rather than the root, to prove the fix
// reaches the language a connection actually attaches with.
func TestDetect_KotlinGradleProjectResolvesKotlin(t *testing.T) {
	// Canonical because Detect's result is (issue #263): on macOS t.TempDir()
	// hands back /var/folders/..., a symlink to /private/var/folders/..., so a
	// raw string compare fails for a reason that has nothing to do with the
	// language. It passes under `make test` only because the Makefile points
	// GOTMPDIR inside the checkout.
	dir := paths.Canonical(t.TempDir())
	writeFiles(t, dir, "settings.gradle.kts", "build.gradle.kts",
		"src/main/kotlin/App.kt")

	root, lang, err := contestedPool(t).Detect(filepath.Join(dir, "src", "main", "kotlin"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != dir {
		t.Errorf("root = %q, want %q", root, dir)
	}
	if lang != "kotlin" {
		t.Fatalf("language = %q, want kotlin — a Kotlin Gradle project must not "+
			"attach as java, which would start jdtls for a project with no Java in it", lang)
	}
}

// manyFiles creates n files named <prefix>NNNNN<ext> under root/rel.
func manyFiles(t *testing.T, root, rel, prefix, ext string, n int) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("%s%05d%s", prefix, i, ext))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStrongLangAt_TruncatedScanIsNotEvidence pins the distinction between a
// count that is COMPLETE and one that merely got as far as the file cap.
//
// The walk is a LIFO over each directory's sorted entries, so it visits
// siblings in reverse-alphabetical order — which has nothing to do with where a
// project keeps its code. When the cap is hit, the counts describe whichever
// prefix of the tree that order happened to reach. Treating that as evidence is
// not a weaker answer, it is a differently-wrong one: here the tie-break sees
// three Kotlin files, never reaches two hundred Java ones, and confidently
// answers kotlin for a Java project. The deterministic language order is the
// honest answer when the scan could not finish, and it is also what plumb
// returned before this tie-break existed — so a truncated scan degrades to the
// old behaviour instead of inventing a new wrong one.
func TestStrongLangAt_TruncatedScanIsNotEvidence(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"settings.gradle.kts", "build.gradle.kts",
		"app/build.gradle.kts", "web/build.gradle.kts",
		"web/src/main/kotlin/com/example/A.kt",
		"web/src/main/kotlin/com/example/B.kt",
		"web/src/main/kotlin/com/example/C.kt",
	)
	for i := range 200 {
		writeFiles(t, dir, fmt.Sprintf("app/src/main/java/com/example/Cls%05d.java", i))
	}
	// Sorts between app/ and web/, and is large enough to exhaust the cap.
	// "docs" pops after "web" and before "app", which is what stops the Java
	// sources being reached at all.
	manyFiles(t, dir, "docs", "page", ".png", extScanMaxFiles+1000)

	if got := contestedPool(t).strongLangAt(dir); got != "java" {
		t.Fatalf("strongLangAt = %q, want java — the project is 200 .java to 3 .kt, "+
			"and a scan that stopped at the %d-file cap must fall back to the "+
			"language order rather than trust the prefix it managed to see",
			got, extScanMaxFiles)
	}
}

// TestStrongLangAt_DepthDoesNotReachAssetTrees pins tieScanDepth's UPPER side.
// The lower side is pinned by the multi-project cases; nothing pinned the upper
// side, and an earlier revision of this branch set the constant one level
// deeper on the reasoning that a spare level was free. It is not free: every
// level is more directories charged against the same file cap.
// src/main/resources/static/css/fonts/ sits exactly at the boundary, so one
// level more descends into the asset directory beneath it and can spend the
// whole budget there before reaching src/main/kotlin — turning a pure-Kotlin
// project back into a jdtls one, which is the bug this file exists to prevent.
func TestStrongLangAt_DepthDoesNotReachAssetTrees(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"settings.gradle.kts", "build.gradle.kts",
		"src/main/kotlin/App.kt", "src/main/kotlin/Greeter.kt")
	manyFiles(t, dir, "src/main/resources/static/css/fonts/files", "f", ".woff", extScanMaxFiles+500)

	if got := contestedPool(t).strongLangAt(dir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin — a pure-Kotlin project whose "+
			"assets sit one level below the scan depth must still be decided by "+
			"its sources", got)
	}
}

// TestExtLangAt_AlsoRefusesSymlinks pins a behaviour change the symlink guard
// makes to the PRE-EXISTING last-resort sniff, which shares the walk. Before,
// a markerless directory of symlinked .py files sniffed as python; now it
// sniffs as nothing and the root attaches LanguageNone. That is the intended
// trade — one rule for what the walk counts, rather than one boundary for the
// tie-break and another for the sniff — but it is a real change to a real
// caller (conn_attach's markerless-root path), so it is pinned rather than left
// to be rediscovered as a regression.
func TestExtLangAt_AlsoRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	writeFiles(t, outside, "a.py", "b.py", "c.py")

	dir := t.TempDir()
	for _, n := range []string{"a.py", "b.py", "c.py"} {
		if err := os.Symlink(filepath.Join(outside, n), filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	p := contestedPool(t, "python")
	if got := p.extLangAt(dir); got != "" {
		t.Fatalf("extLangAt = %q, want \"\" — every .py entry is a symlink out of "+
			"dir, and the walk counts only what lives beneath it", got)
	}
	// The same files, real rather than linked, still sniff as python — so the
	// guard is about symlinks and has not simply broken the sniff.
	realDir := t.TempDir()
	writeFiles(t, realDir, "a.py", "b.py", "c.py")
	if got := p.extLangAt(realDir); got != "python" {
		t.Fatalf("extLangAt = %q, want python for real files", got)
	}
}

// TestExtLangAt_StillAnswersFromATruncatedScan pins the other half of the
// truncation rule: the two callers of sniffCounts treat a partial count
// OPPOSITELY, and that asymmetry is deliberate.
//
// strongLangAt discards a truncated count, because it is choosing between two
// named candidates and a prefix of the tree is differently-wrong rather than
// merely vague. extLangAt keeps it, because its alternative is LanguageNone — a
// root with no marker at all, attaching no language server and losing every LSP
// tool. A coarse guess off the first files beats that. Unifying the two would
// look like a tidy-up and would silently cost every large markerless repo its
// language, so it is pinned here rather than left to the doc comment.
func TestExtLangAt_StillAnswersFromATruncatedScan(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "a.py", "b.py", "c.py")
	manyFiles(t, dir, "assets", "blob", ".bin", extScanMaxFiles+500)

	counts, truncated := contestedPool(t, "python").sniffCounts(dir, extScanDepth, nil)
	if !truncated {
		t.Fatalf("precondition: scan was not truncated (counts=%v) — the fixture no "+
			"longer exceeds the %d-file cap, so this test proves nothing", counts, extScanMaxFiles)
	}
	if got := contestedPool(t, "python").extLangAt(dir); got != "python" {
		t.Fatalf("extLangAt = %q, want python — the last-resort sniff must still "+
			"answer from a truncated scan; returning \"\" costs the root its "+
			"language server entirely", got)
	}
}

// TestSniffCounts_RefusesSymlinksOutOfTheCandidate is the boundary guard on the
// tie-break's scan. Breaking a tie by "the language owning the most source files
// BENEATH dir" is exactly the shape that walks out of a directory, and plumb has
// shipped that class of bug before (#264). Both escape routes are pinned here,
// and each half fails against a different regression:
//
//   - a symlinked FILE is not counted. Before the guard this was live, not
//     hypothetical: the count keys on de.Name() alone and never stats the
//     target, so `A.kt -> /anywhere/A.kt` added a Kotlin file to a project
//     holding none. Deleting the `de.Type()&os.ModeSymlink` guard turns this
//     subtest red.
//   - a symlinked DIRECTORY is not descended. This already held incidentally,
//     because DirEntry.IsDir is false for a link, and this subtest is what
//     stops a later "fix" from resolving entries through os.Stat and quietly
//     restoring the escape. Replacing the guard plus the de.IsDir() branch with
//     a symlink-following os.Stat turns this subtest red.
//
// Both cases stack the OUTSIDE evidence so it would win the tie if it counted,
// and leave one real in-boundary Java file — so the assertion can only pass by
// the scan genuinely staying beneath the candidate directory.
func TestSniffCounts_RefusesSymlinksOutOfTheCandidate(t *testing.T) {
	t.Run("a symlinked file is not counted", func(t *testing.T) {
		outside := t.TempDir()
		writeFiles(t, outside, "A.kt", "B.kt", "C.kt")

		dir := t.TempDir()
		writeFiles(t, dir, "build.gradle.kts", "src/main/java/Only.java")
		links := filepath.Join(dir, "src", "main", "kotlin")
		if err := os.MkdirAll(links, 0o755); err != nil {
			t.Fatal(err)
		}
		// Three links against one real Java file: counted, kotlin takes the tie.
		for _, n := range []string{"A.kt", "B.kt", "C.kt"} {
			if err := os.Symlink(filepath.Join(outside, n), filepath.Join(links, n)); err != nil {
				t.Fatal(err)
			}
		}

		if got := contestedPool(t).strongLangAt(dir); got != "java" {
			t.Fatalf("strongLangAt = %q, want java — the .kt entries are symlinks "+
				"to %s and name no file beneath the candidate directory", got, outside)
		}
	})

	t.Run("a symlinked directory is not descended", func(t *testing.T) {
		outside := t.TempDir()
		writeFiles(t, outside, "src/A.kt", "src/B.kt", "src/C.kt", "src/D.kt")

		dir := t.TempDir()
		writeFiles(t, dir, "build.gradle.kts", "src/main/java/Only.java")
		if err := os.Symlink(filepath.Join(outside, "src"), filepath.Join(dir, "src", "main", "kotlin")); err != nil {
			t.Fatal(err)
		}

		if got := contestedPool(t).strongLangAt(dir); got != "java" {
			t.Fatalf("strongLangAt = %q, want java — src/main/kotlin is a symlink "+
				"to %s and the scan must not descend through it", got, outside)
		}
	})
}

// TestDominantAmong_IgnoresLanguagesOutsideTheTie pins the restriction that
// keeps an unrelated language out of a tie it was never part of: only the
// contested candidates may win, however many files another language owns.
func TestDominantAmong_IgnoresLanguagesOutsideTheTie(t *testing.T) {
	counts := map[string]int{"go": 500, "java": 2, "kotlin": 7}
	if got := dominantAmong(counts, []string{"java", "kotlin"}); got != "kotlin" {
		t.Fatalf("dominantAmong = %q, want kotlin", got)
	}
	if got := dominantAmong(counts, []string{"java"}); got != "java" {
		t.Fatalf("dominantAmong = %q, want java", got)
	}
	if got := dominantAmong(map[string]int{"go": 9}, []string{"java", "kotlin"}); got != "" {
		t.Fatalf("dominantAmong = %q, want \"\" when no candidate owns a file", got)
	}
}
