//go:build clients || clients_e2e || clients_conformance

package clientsmoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunPlumbSetup_DoesNotDirtyThePackageDirectory pins the harness side of
// the instruction-file write: `plumb setup` puts its managed block into the
// CURRENT directory's instruction file, so a setup run without an explicit
// working directory used to inherit this test binary's cwd — the
// cmd/clientsmoke source directory — and drop an untracked AGENTS.md (or
// CLAUDE.md/GEMINI.md) into the developer's checkout on every harness run.
// runPlumbSetup must instead run plumb inside the isolated HOME, and the
// block must land there, never silently suppressed.
func TestRunPlumbSetup_DoesNotDirtyThePackageDirectory(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal("resolve package directory:", err)
	}
	for _, tc := range []struct {
		client string
		file   string // the project-level instruction file this client's setup writes
	}{
		{client: "codex", file: "AGENTS.md"},
		{client: "claude-code", file: "CLAUDE.md"},
		{client: "gemini", file: "GEMINI.md"},
	} {
		t.Run(tc.client, func(t *testing.T) {
			tmpHome := mkTmpHome(t)
			env := isolatedEnv(tmpHome)

			runPlumbSetup(t, env, "setup", tc.client)

			if _, err := os.Stat(filepath.Join(pkgDir, tc.file)); err == nil {
				t.Fatalf("plumb setup %s wrote %s into the package directory — untracked debris in the repo tree", tc.client, tc.file)
			} else if !os.IsNotExist(err) {
				t.Fatal("stat package directory:", err)
			}

			block, err := os.ReadFile(filepath.Join(tmpHome, tc.file))
			if err != nil {
				t.Fatalf("plumb setup %s did not write %s into the isolated HOME: %v", tc.client, tc.file, err)
			}
			if !strings.Contains(string(block), "<!-- plumb:managed:start") {
				t.Fatalf("%s in the isolated HOME carries no plumb managed block — the write was suppressed, not relocated", tc.file)
			}
		})
	}
}
