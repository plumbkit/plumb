package cli

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
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

// TestSharedIdentityRefusalSaysRetryOnce pins the one line that tells an agent
// what to do about a call the hook failed to stamp. With the hook installed,
// the refusal still arrives whenever its probe of the daemon misses under load
// (issue #556), and an agent told only to install the hook would conclude the
// hook it already has is broken. The refusal still has to name the real remedy
// (the first assertion), so this is an addition to that text, not a
// replacement, and it must not drift back to recommending session_id.
func TestSharedIdentityRefusalSaysRetryOnce(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	root := freshTempDir(t)
	mustGitDir(t, root)
	newSharedConn(t, s, root, "conv-x", "conv-y")

	err := s.refuseSharedStateChange(context.Background(), "write_file", "")
	if err == nil {
		t.Fatal("precondition: an unidentified write on a shared connection must be refused")
	}
	for _, want := range []string{"no logical-agent identity", "plumb hooks install claude-code", "retry once"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
}
