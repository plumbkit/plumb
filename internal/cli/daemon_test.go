package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"
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
