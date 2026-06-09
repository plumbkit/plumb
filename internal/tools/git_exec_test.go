package tools

import (
	"strings"
	"testing"
)

// TestFindGitRoot_EmptyPathRefuses is the last line of defence against the
// cross-session leak: an empty path must error, never resolve to the daemon's
// cwd (os.Getwd), which is shared across connections and may belong to an
// unrelated repository.
func TestFindGitRoot_EmptyPathRefuses(t *testing.T) {
	_, err := findGitRoot("")
	if err == nil || !strings.Contains(err.Error(), "no repository path") {
		t.Fatalf("findGitRoot(\"\") must refuse, got %v", err)
	}
}
