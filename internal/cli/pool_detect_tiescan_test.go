package cli

import (
	"fmt"
	"testing"
)

// TestStrongLangAt_LargeResourceTreeDoesNotStarveTheTieBreak is the regression
// test for the tie-break's starvation bug. The walk charged every file it
// examined against extScanMaxFiles — the content sniff's budget — and it is a
// LIFO over each directory's sorted entries, so siblings pop in
// reverse-alphabetical order and src/main/resources is examined before
// src/main/kotlin. A resources tree larger than the cap exhausted the budget
// before a single source file was reached; strongLangAt saw truncated=true,
// discarded the count, and fell back to the deterministic language order —
// java. A pure-Kotlin project re-attached jdtls once its resources tree
// crossed a size threshold, which is the very shape #341's tie-break exists to
// resolve by content.
func TestStrongLangAt_LargeResourceTreeDoesNotStarveTheTieBreak(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "settings.gradle.kts", "build.gradle.kts")
	manyFiles(t, dir, "src/main/kotlin", "F", ".kt", 20)
	manyFiles(t, dir, "src/main/resources", "m", ".properties", 2500)

	if got := contestedPool(t).strongLangAt(dir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin — a pure-Kotlin Gradle project "+
			"must not be decided by which directory the tie-break's scan happened "+
			"to exhaust its budget in", got)
	}
}

// TestStrongLangAt_TieBreakBudgetSurvivesAModerateNonSourceTree pins the LOWER
// side of tieScanMaxFiles: a non-source tree large enough to have starved the
// old, sniff-sized budget must not starve the tie-break's own. docs/ is not a
// pruned name, so its files are charged against the budget like any other tree
// — the walk can only answer by finishing. Lowering tieScanMaxFiles toward the
// old extScanMaxFiles truncates it, and the fallback hands this Kotlin project
// to java.
func TestStrongLangAt_TieBreakBudgetSurvivesAModerateNonSourceTree(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"settings.gradle.kts", "build.gradle.kts",
		"web/src/main/kotlin/com/example/A.kt",
		"web/src/main/kotlin/com/example/B.kt",
		"app/src/main/java/com/example/Only.java")
	manyFiles(t, dir, "docs", "page", ".png", 5000)

	if got := contestedPool(t).strongLangAt(dir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin — 5000 unrecognised files under an "+
			"unpruned directory exhausted the old budget twice over, but the "+
			"tie-break's own budget must finish the walk and count 2 Kotlin "+
			"against 1 Java", got)
	}
}

// TestStrongLangAt_TieBreakStillTruncatesAboveItsBudget pins the UPPER side of
// tieScanMaxFiles: the budget is a work bound, not a target, and a tree over
// it must still truncate and degrade to the deterministic language order.
// docs/ is unpruned, and the walk is a LIFO over sorted listings, so it pops
// web/ (200 Kotlin files), spends the budget in docs/, and never reaches the
// 3 Java files in app/. A completed walk would answer kotlin — 200 against 3 —
// so this assertion can only hold while the walk genuinely stops at the cap.
func TestStrongLangAt_TieBreakStillTruncatesAboveItsBudget(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "settings.gradle.kts", "build.gradle.kts")
	for i := range 200 {
		writeFiles(t, dir, fmt.Sprintf("web/src/main/kotlin/com/example/Cls%05d.kt", i))
	}
	writeFiles(t, dir,
		"app/src/main/java/com/example/A.java",
		"app/src/main/java/com/example/B.java",
		"app/src/main/java/com/example/C.java")
	manyFiles(t, dir, "docs", "page", ".png", tieScanMaxFiles+1000)

	if got := contestedPool(t).strongLangAt(dir); got != "java" {
		t.Fatalf("strongLangAt = %q, want java — the walk stopped at the %d-file "+
			"cap, and a truncated tie-break must fall back to the language order "+
			"rather than trust the prefix it managed to see", got, tieScanMaxFiles)
	}
}

// TestStrongLangAt_PrunedResourceTreeCannotStarveTheTieBreak pins pruning as
// starvation-prevention: a resources tree LARGER than even the tie-break's own
// budget cannot exhaust it, because the walk never descends into one. Without
// pruning, reverse-alphabetical pop order reaches resources before kotlin and
// the budget dies there; the fallback then hands this Kotlin project to java.
func TestStrongLangAt_PrunedResourceTreeCannotStarveTheTieBreak(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "settings.gradle.kts", "build.gradle.kts",
		"src/main/kotlin/App.kt", "src/main/kotlin/Greeter.kt")
	manyFiles(t, dir, "src/main/resources", "m", ".properties", tieScanMaxFiles+500)

	if got := contestedPool(t).strongLangAt(dir); got != "kotlin" {
		t.Fatalf("strongLangAt = %q, want kotlin — a resources tree larger than "+
			"the tie-break budget must be pruned, not scanned", got)
	}
}

// TestStrongLangAt_TieBreakPrunesKnownNonSourceDirs pins each pruned directory
// name individually: files under src/main/resources, assets/ or res/ cast no
// vote, even source files. Each subtest stacks 3 Java files inside the pruned
// directory against 2 Kotlin sources outside it — unpruned, the Java files
// outvote the sources and take the tie.
func TestStrongLangAt_TieBreakPrunesKnownNonSourceDirs(t *testing.T) {
	for _, name := range []string{"resources", "assets", "res"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, "settings.gradle.kts", "build.gradle.kts",
				"src/main/kotlin/A.kt", "src/main/kotlin/B.kt")
			writeFiles(t, dir,
				"src/main/"+name+"/one.java",
				"src/main/"+name+"/two.java",
				"src/main/"+name+"/three.java")

			if got := contestedPool(t).strongLangAt(dir); got != "kotlin" {
				t.Fatalf("strongLangAt = %q, want kotlin — src/main/%s is a "+
					"conventional non-source directory; its files must not vote in "+
					"the tie-break", got, name)
			}
		})
	}
}
