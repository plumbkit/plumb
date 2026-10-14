package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_history_callmemo_test.go — inside one context_for_task call the project of
// each directory is detected once (tools.CallMemo, PLAN-548); outside one, as for
// every write, it is detected on each ask. The verdict is the same either way.

func memoTestSession(t *testing.T) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// assertSameDir compares by identity, not by string: a TempDir path and the root
// Detect returns can spell one directory differently (macOS /var → /private/var).
func assertSameDir(t *testing.T, got, want, why string) {
	t.Helper()
	gi, err1 := os.Stat(got)
	wi, err2 := os.Stat(want)
	if err1 != nil || err2 != nil || !os.SameFile(gi, wi) {
		t.Errorf("%s: root = %q, want %q", why, got, want)
	}
}

func TestHistoryRootFor_DetectsEachDirectoryOncePerContextCall(t *testing.T) {
	s := memoTestSession(t)
	root := t.TempDir()
	pkg := filepath.Join(root, "pkg")
	writeTree(t, root, map[string]string{"go.mod": "module x\n", "pkg/a.go": "package pkg\n", "pkg/b.go": "package pkg\n"})
	a, b := filepath.Join(pkg, "a.go"), filepath.Join(pkg, "b.go")

	call := tools.WithCallMemo(context.Background())
	assertSameDir(t, s.historyRootFor(call, a), root, "first ask")

	// pkg becomes a project of its own part-way through the call. The call's
	// second ask about that directory keeps the answer it already has: it did not
	// detect again.
	writeTree(t, root, map[string]string{"pkg/go.mod": "module x/pkg\n"})
	assertSameDir(t, s.historyRootFor(call, b), root, "a second ask in the same call (remembered)")

	// The next call, and any ask outside a call, sees the marker.
	assertSameDir(t, s.historyRootFor(tools.WithCallMemo(context.Background()), b), pkg, "the next call")
	assertSameDir(t, s.historyRootFor(context.Background(), a), pkg, "an ask outside a call (every write)")
}

// TestChangeSensitive_CallMemoKeepsEveryVerdict: the memo changes how often the
// decision detects, never what it decides, including a change whose source lies
// in another project whose own globs make it sensitive.
func TestChangeSensitive_CallMemoKeepsEveryVerdict(t *testing.T) {
	s := memoTestSession(t)
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, map[string]string{"go.mod": "module a\n", "main.go": "package main\n", "sub/util.go": "package sub\n"})
	writeTree(t, b, map[string]string{
		"go.mod":             "module b\n",
		"main.go":            "package main\n",
		"key.vault":          "secret\n",
		".plumb/config.toml": "[history]\nsensitive_globs = [\"*.vault\"]\n",
	})
	cases := []struct{ name, path, from string }{
		{"an ordinary file", filepath.Join(a, "main.go"), ""},
		{"a default glob", filepath.Join(a, "server.pem"), ""},
		{"a glob of its own project", filepath.Join(b, "key.vault"), ""},
		{"the same name where no glob says so", filepath.Join(a, "key.vault"), ""},
		{"moved out of another project's sensitive file", filepath.Join(a, "sub/util.go"), filepath.Join(b, "key.vault")},
		{"moved in from an ordinary file", filepath.Join(b, "main.go"), filepath.Join(a, "main.go")},
	}

	call := tools.WithCallMemo(context.Background())
	var sensitive, plain int
	for _, tc := range cases {
		want := s.changeSensitive(context.Background(), tc.path, tc.from)
		// Asked twice in the call, so the second answer comes from the memo.
		for range 2 {
			if got := s.changeSensitive(call, tc.path, tc.from); got != want {
				t.Errorf("%s: verdict in a call = %v, outside one = %v", tc.name, got, want)
			}
		}
		if want {
			sensitive++
		} else {
			plain++
		}
	}
	// Controls: the cases span both verdicts, and the cross-project source really
	// is sensitive only through its own project's glob.
	if sensitive == 0 || plain == 0 {
		t.Fatalf("control: %d sensitive and %d plain verdicts; the cases must span both", sensitive, plain)
	}
	if !s.changeSensitive(context.Background(), cases[4].path, cases[4].from) || s.changeSensitive(context.Background(), cases[3].path, "") {
		t.Fatal("control: the cross-project case must be sensitive through b's glob alone")
	}
}
