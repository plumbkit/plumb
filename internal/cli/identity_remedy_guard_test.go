package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// literalJoin matches the seam between two concatenated Go string literals
// ("...a " + "b..."), so a banned phrase split across source lines is still
// found once the seams are removed.
var literalJoin = regexp.MustCompile(`"\s*\+\s*"`)

// TestNoMessageRecommendsSessionIDPerCall scans plumb's own source for the
// remedy that stopped being true: a session_start.session_id identifies only
// that call, and once two agents have declared one, a later call without a
// per-call identity is refused. Messages that told agents to "pass
// session_start.session_id on every call" sent them straight into that
// refusal. Every such message now uses tools.PerCallIdentityRemedy.
func TestNoMessageRecommendsSessionIDPerCall(t *testing.T) {
	banned := []string{
		"session_id on every call",
		"identify yourself with session_start.session_id",
		"identify itself (session_start.session_id",
	}
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
		joined := literalJoin.ReplaceAllString(string(src), "")
		for _, b := range banned {
			if strings.Contains(joined, b) {
				hits = append(hits, path+": "+b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("source still recommends session_id as a per-call identity (use tools.PerCallIdentityRemedy): %v", hits)
	}
}
