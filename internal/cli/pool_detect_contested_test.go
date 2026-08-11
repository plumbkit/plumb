package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
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

// TestStrongLangAt_UncontestedMarkerSkipsTheSniff pins that the tie-break is
// confined to a genuinely contested directory: a marker only one language claims
// resolves on the marker alone, so the common case pays no filesystem scan and
// cannot be swayed by whatever sources happen to sit nearby.
func TestStrongLangAt_UncontestedMarkerSkipsTheSniff(t *testing.T) {
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
	dir := t.TempDir()
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
