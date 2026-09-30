package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoMessageRecommendsSessionIDPerCall scans plumb's own source for the
// remedy that stopped being true: a session_start.session_id identifies only
// that call, and once two agents have declared one, a later call without a
// per-call identity is refused. Messages that told agents to "pass
// session_start.session_id on every call" sent them straight into that
// refusal. Every such message now uses tools.PerCallIdentityRemedy.
func TestNoMessageRecommendsSessionIDPerCall(t *testing.T) {
	const banned = "session_id on every call"
	root := filepath.Join("..")
	var hits []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), banned) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("source still recommends %q (use tools.PerCallIdentityRemedy): %v", banned, hits)
	}
}
