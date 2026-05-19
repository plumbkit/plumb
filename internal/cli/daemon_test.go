package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSeedPathFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args string
		want string
	}{
		{
			name: "uri with file scheme",
			args: `{"uri":"file:///tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "uri without scheme",
			args: `{"uri":"/tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "path field",
			args: `{"path":"/tmp/foo.go"}`,
			want: "/tmp/foo.go",
		},
		{
			name: "root field (list_files)",
			args: `{"root":"/tmp/proj"}`,
			want: "/tmp/proj",
		},
		{
			name: "workspace field (session_start)",
			args: `{"workspace":"/tmp/proj"}`,
			want: "/tmp/proj",
		},
		{
			name: "paths array (read_multiple_files)",
			args: `{"paths":["/tmp/a.go","/tmp/b.go"]}`,
			want: "/tmp/a.go",
		},
		{
			name: "operations array (transaction_apply)",
			args: `{"operations":[{"path":"/tmp/a.go","edits":[]},{"path":"/tmp/b.go","edits":[]}]}`,
			want: "/tmp/a.go",
		},
		{
			name: "uri preferred over path",
			args: `{"uri":"file:///tmp/from-uri","path":"/tmp/from-path"}`,
			want: "/tmp/from-uri",
		},
		{
			name: "path preferred over paths array",
			args: `{"path":"/tmp/single","paths":["/tmp/multi"]}`,
			want: "/tmp/single",
		},
		{
			name: "empty paths array falls through",
			args: `{"paths":[]}`,
			want: "",
		},
		{
			name: "empty operations array falls through",
			args: `{"operations":[]}`,
			want: "",
		},
		{
			name: "no recognised field",
			args: `{"unrelated":"value"}`,
			want: "",
		},
		{
			name: "malformed JSON",
			args: `{not-json`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := seedPathFromArgs(json.RawMessage(tt.args))
			if got != tt.want {
				t.Errorf("seedPathFromArgs(%s) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestWorkspaceFromArgs_NestedShapes guards against the regression where
// transaction_apply and read_multiple_files were invisible to workspace
// attribution because their paths are nested. Without this, stats for these
// tools fall back to the connection's primary workspace and are dropped for
// sessions that never resolved one.
func TestWorkspaceFromArgs_NestedShapes(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	pool := detectTestPool()

	cases := []struct {
		name string
		args string
	}{
		{
			name: "transaction_apply with one operation",
			args: `{"operations":[{"path":"` + dir + `/sub/foo.go","edits":[]}]}`,
		},
		{
			name: "transaction_apply with multiple operations",
			args: `{"operations":[{"path":"` + dir + `/sub/foo.go","edits":[]},{"path":"` + dir + `/bar.go","edits":[]}]}`,
		},
		{
			name: "read_multiple_files",
			args: `{"paths":["` + dir + `/sub/foo.go","` + dir + `/bar.go"]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceFromArgs(pool, json.RawMessage(tc.args))
			if got != dir {
				t.Errorf("workspaceFromArgs = %q, want %q", got, dir)
			}
		})
	}
}

func TestDaemonScansTxlogSynchronouslyOnAttach(t *testing.T) {
	called := make(chan struct{})
	release := make(chan struct{})
	scan := func(string) {
		close(called)
		<-release
	}

	done := make(chan struct{})
	go func() {
		recoverWorkspaceTxlog("/workspace", scan)
		close(done)
	}()

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("scanTxlog was not called")
	}

	select {
	case <-done:
		t.Fatal("recoverWorkspaceTxlog returned before txlog scan completed")
	default:
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recoverWorkspaceTxlog did not return after txlog scan completed")
	}
}

// TestAutoAttach_SynthesiseRootOnDetectFailure verifies that when pool.Detect
// fails (no project marker) and AutoAttach is enabled, pool.SynthesiseRoot
// returns the nearest .git ancestor — matching what OnBeforeTool would use.
func TestAutoAttach_SynthesiseRootOnDetectFailure(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "pkg", "myapp")
	mustMkdir(t, sub)

	pool := detectTestPool()

	// Detect must fail — there is no go.mod or pyproject.toml.
	if _, _, err := pool.Detect(sub); err == nil {
		t.Fatal("Detect: expected error for directory with no language marker, got nil")
	}

	// SynthesiseRoot must walk up to the .git-bearing ancestor.
	got := pool.SynthesiseRoot(sub)
	if got != root {
		t.Errorf("SynthesiseRoot = %q, want %q (.git ancestor)", got, root)
	}
}

// TestAutoAttach_SynthesiseRootNoGit verifies that when no .git exists anywhere
// in the tree, SynthesiseRoot falls back to the seed directory itself.
func TestAutoAttach_SynthesiseRootNoGit(t *testing.T) {
	root := freshTempDir(t)
	sub := filepath.Join(root, "a", "b", "c")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	if got != sub {
		t.Errorf("SynthesiseRoot = %q, want seed %q", got, sub)
	}
}

// TestAutoAttach_MaterialisePlumbDir verifies that materialisePlumbDir creates
// <root>/.plumb/ and is idempotent (calling twice does not error).
func TestAutoAttach_MaterialisePlumbDir(t *testing.T) {
	root := freshTempDir(t)

	if err := materialisePlumbDir(root); err != nil {
		t.Fatalf("first call: %v", err)
	}
	plumbDir := filepath.Join(root, ".plumb")
	if info, err := os.Stat(plumbDir); err != nil || !info.IsDir() {
		t.Fatalf(".plumb/ not created at %s", plumbDir)
	}

	// Idempotent — must not fail when the directory already exists.
	if err := materialisePlumbDir(root); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}
}

// TestAutoAttach_MaterialisePlumbDir_NestedRoot verifies creation works when
// intermediate directories in the synthetic root path do not yet exist.
func TestAutoAttach_MaterialisePlumbDir_NestedRoot(t *testing.T) {
	root := freshTempDir(t)
	nested := filepath.Join(root, "a", "b", "c")
	// Do NOT create the nested directories — materialisePlumbDir must do it.
	if err := materialisePlumbDir(nested); err != nil {
		t.Fatalf("materialisePlumbDir: %v", err)
	}
	plumbDir := filepath.Join(nested, ".plumb")
	if info, err := os.Stat(plumbDir); err != nil || !info.IsDir() {
		t.Fatalf(".plumb/ not created under nested path %s", plumbDir)
	}
}
