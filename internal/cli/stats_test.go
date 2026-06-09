package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestRunStats_ShowsRows(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	db, err := stats.Open()
	if err != nil {
		t.Fatalf("Open DB: %v", err)
	}
	// Use os.TempDir() directly so the path is outside the plumb workspace tree;
	// t.TempDir() lands under GOTMPDIR (.testcache/) which Detect() walks up through.
	ws, err := os.MkdirTemp(os.TempDir(), "plumb-test-ws-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	if err := db.Record(stats.Call{
		SessionID: "sess-1",
		Workspace: ws,
		Tool:      "read_file",
		CalledAt:  time.Now(),
		Success:   true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	db.Close()

	oldWorkspaceFlag, oldLimit := statsFlagWorkspace, statsFlagLimit
	statsFlagWorkspace, statsFlagLimit = ws, 5
	defer func() {
		statsFlagWorkspace, statsFlagLimit = oldWorkspaceFlag, oldLimit
	}()

	out := captureStdout(t, func() {
		if err := runStats(nil, nil); err != nil {
			t.Fatalf("runStats: %v", err)
		}
	})

	if strings.Contains(out, "No statistics") {
		t.Fatalf("runStats reported no statistics:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Fatalf("runStats output did not include tool call:\n%s", out)
	}
}

func TestRunStats_NoStatsPrintsLogo(t *testing.T) {
	// PrintLogo is guarded by a process-global once flag; reset it so this test
	// deterministically observes the banner regardless of earlier tests in the
	// package having already printed it.
	logoPrinted = false
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Use os.TempDir() so the workspace is outside the plumb workspace tree.
	ws, err := os.MkdirTemp(os.TempDir(), "plumb-test-ws-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	oldWorkspaceFlag, oldLimit := statsFlagWorkspace, statsFlagLimit
	statsFlagWorkspace, statsFlagLimit = ws, 5
	defer func() {
		statsFlagWorkspace, statsFlagLimit = oldWorkspaceFlag, oldLimit
	}()

	out := captureStdout(t, func() {
		if err := runStats(nil, nil); err != nil {
			t.Fatalf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "╭─╮") {
		t.Fatalf("runStats output did not include logo:\n%s", out)
	}
	if !strings.Contains(out, "No statistics recorded yet") {
		t.Fatalf("runStats output did not include no-statistics message:\n%s", out)
	}
}

func TestResolveCLIWorkspace_NestedDirectoryUsesProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCLIWorkspace(nested, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("resolveCLIWorkspace(%s) = %s, want %s", nested, got, root)
	}
}

func TestResolveCLIWorkspace_NonProjectDirectoryPreserved(t *testing.T) {
	// Use os.TempDir() so the path is outside the plumb workspace tree;
	// t.TempDir() lands under GOTMPDIR (.testcache/) which Detect() walks up through.
	dir, err := os.MkdirTemp(os.TempDir(), "plumb-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	got, err := resolveCLIWorkspace(dir, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("resolveCLIWorkspace(non-project) = %s, want %s", got, dir)
	}
}

func TestRunStats_ExplicitNestedWorkspaceUsesRootWorkspaceFilter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	root := "/projects/my-project"

	db, err := stats.Open()
	if err != nil {
		t.Fatalf("Open DB: %v", err)
	}
	if err := db.Record(stats.Call{
		SessionID: "sess-1",
		Workspace: root,
		Tool:      "read_file",
		CalledAt:  time.Now(),
		Success:   true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	db.Close()

	// Mocking directory structure for resolveCLIWorkspace
	tempRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tempRoot, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	tempChild := filepath.Join(tempRoot, "child")
	if err := os.MkdirAll(tempChild, 0o755); err != nil {
		t.Fatal(err)
	}

	oldWorkspaceFlag, oldLimit := statsFlagWorkspace, statsFlagLimit
	statsFlagWorkspace, statsFlagLimit = tempChild, 5
	defer func() {
		statsFlagWorkspace, statsFlagLimit = oldWorkspaceFlag, oldLimit
	}()

	out := captureStdout(t, func() {
		if err := runStats(nil, nil); err != nil {
			t.Fatalf("runStats: %v", err)
		}
	})

	if !strings.Contains(out, "No statistics for workspace") {
		t.Fatalf("runStats should have reported no statistics for resolved tempRoot:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing stdout pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading stdout pipe: %v", err)
	}
	return string(out)
}
