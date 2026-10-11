package collab

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestScopeGuard_OnlyScopeGoFiltersByTargetWorkspace keeps the daemon-level
// store's workspace filter in ONE place. A read that writes its own
// `target_workspace = ?` bypasses the reader's consent and family check in
// scope.go: with the term spelled by hand it would read every row aimed at the
// workspace, from any project, whatever the reader agreed to. Every predicate
// on the column therefore lives in scope.go, and a new read takes a readScope.
func TestScopeGuard_OnlyScopeGoFiltersByTargetWorkspace(t *testing.T) {
	predicate := regexp.MustCompile(`target_workspace\s*(=|!=|IN\b|LIKE\b)`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if predicate.MatchString(line) && f != "scope.go" {
				t.Errorf("%s:%d filters on target_workspace outside scope.go — take a readScope from Store.scope instead:\n\t%s",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	// Positive control: the guard must actually see the file that holds the term.
	data, err := os.ReadFile("scope.go")
	if err != nil || !predicate.Match(data) {
		t.Fatalf("the guard's pattern does not match scope.go's own term (err %v) — it would pass vacuously", err)
	}
	if checked < 5 {
		t.Fatalf("only %d source files scanned; the guard is not looking at the package", checked)
	}
}
