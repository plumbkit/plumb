package txlog

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// payloadEcho bounds how much of a payload is echoed into a failure message.
// textfmt.Ellipsis is rune-safe, which matters here: a fuzzer feeds this
// arbitrary bytes, and a byte-sliced copy would emit replacement characters
// mid-sequence in the very message meant to reproduce the finding.
const payloadEcho = 60

// FuzzScanReplay fuzzes the transaction-journal REPLAY path — the one part of
// txlog that parses input plumb did not write. Begin/Record/Commit run inside a
// live transaction, but Scan reads a manifest left on disk and acts on it, and
// `.plumb/tx-log/` is an ordinary directory inside the workspace: a cloned
// repository can ship one, and the daemon replays it unconditionally on attach
// (internal/cli/conn_attach.go, conn_repin.go).
//
// The invariant asserted is CONFINEMENT: replaying any manifest must never
// create or modify a file outside the workspace whose journal it is. Rollback
// exists to undo writes the transaction machinery itself made, and those are
// boundary-checked before they happen; a replay that trusts the manifest's own
// `path` field inherits none of that checking.
//
// Harness safety bound: a fuzzer that hands os.WriteFile arbitrary absolute
// paths would write over the developer's real filesystem, so an input whose
// manifest names a path outside the per-iteration temp root is SKIPPED, not
// executed. The escape is still fully expressible — the recorded corpus entries
// in testdata/fuzz/FuzzScanReplay/ address the sibling `outside/` directory via
// the {{ROOT}} placeholder — so the property under test is unchanged; only
// genuinely unbounded targets are refused.
func FuzzScanReplay(f *testing.F) {
	// Benign: a well-formed manifest restoring a file inside the workspace.
	f.Add(`{"tx_id":"1","started_at":"2000-01-01T00:00:00Z","workspace":"{{ROOT}}/ws",`+
		`"ops":[{"n":0,"path":"{{ROOT}}/ws/src/main.go","perm":420,"snapshotted":true}]}`, []byte("package main\n"), uint8(1))
	// Several ops, mixed snapshotted flags and out-of-order indices.
	f.Add(`{"started_at":"2000-01-01T00:00:00Z","ops":[`+
		`{"n":2,"path":"{{ROOT}}/ws/a","perm":420,"snapshotted":true},`+
		`{"n":0,"path":"{{ROOT}}/ws/b","perm":420,"snapshotted":false},`+
		`{"n":1,"path":"{{ROOT}}/ws/c","perm":420,"snapshotted":true}]}`, []byte("multi"), uint8(4))
	// The escape shapes are RECORDED CRASHERS and live in
	// testdata/fuzz/FuzzScanReplay/ rather than here, so they can be deleted as
	// one unit when the confinement defect they document is fixed.
	//
	// Structurally hostile manifests: truncated, wrong types, duplicate keys,
	// enormous and negative snapshot indices, empty and null op lists.
	f.Add(`{"started_at":"2000-01-01T00:00:00Z","ops":[{"n":0,"path":"{{ROOT}}/ws/x"`, []byte(""), uint8(0))
	f.Add(`{"ops":null}`, []byte(""), uint8(0))
	f.Add(`{"started_at":"0001-01-01T00:00:00Z","ops":[{"n":-1,"path":"{{ROOT}}/ws/x","snapshotted":true}]}`, []byte("neg"), uint8(1))
	f.Add(`{"started_at":"2000-01-01T00:00:00Z","ops":[{"n":9223372036854775807,"path":"{{ROOT}}/ws/x","snapshotted":true}]}`, []byte("big"), uint8(1))
	f.Add(`{"started_at":"2000-01-01T00:00:00Z","ops":[{"n":0,"n":0,"path":"{{ROOT}}/ws/x","path":"{{ROOT}}/ws/dup","snapshotted":true}]}`, []byte("dup"), uint8(1))
	f.Add(`not json at all`, []byte(""), uint8(0))
	f.Add(``, []byte(""), uint8(0))

	f.Fuzz(func(t *testing.T, manifest string, snapshot []byte, snapCount uint8) {
		root := t.TempDir()
		// EvalSymlinks: on macOS t.TempDir() sits under /var, a symlink to
		// /private/var, and the confinement comparison must be on resolved paths.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			root = resolved
		}
		ws := filepath.Join(root, "ws")
		outside := filepath.Join(root, "outside")
		txDir := filepath.Join(ws, ".plumb", "tx-log", "orphan")
		mkdirAll(t, txDir)
		mkdirAll(t, filepath.Join(ws, "src"))
		mkdirAll(t, outside)

		manifest = strings.ReplaceAll(manifest, "{{ROOT}}", root)
		if !confinedToRoot(manifest, root) {
			t.Skip("manifest names a path outside the temp root; see the harness safety bound")
		}

		writeFile(t, filepath.Join(ws, "src", "main.go"), []byte("in-workspace\n"))
		writeFile(t, filepath.Join(outside, "sentinel.txt"), []byte("untouched\n"))
		writeFile(t, filepath.Join(txDir, "manifest.json"), []byte(manifest))
		// Snapshot files are addressed as "<n>-before"; provide a bounded run of
		// them so a manifest naming any small index finds content to restore.
		for i := range int(snapCount % 8) {
			writeFile(t, filepath.Join(txDir, strconv.Itoa(i)+"-before"), snapshot)
		}

		before := treeSnapshot(t, outside)

		// liveCutoff is now: every orphan is older, so every manifest is replayed.
		// The guard stands in for the session's PathPolicy, which in production
		// admits the workspace plus any configured extra roots.
		Scan(ws, time.Now(), confinedTo(ws))

		after := treeSnapshot(t, outside)
		for path, content := range after {
			old, existed := before[path]
			switch {
			case !existed:
				t.Errorf("txlog replay CREATED a file outside the workspace: %s (content %q)\n"+
					"manifest: %s", path, textfmt.Ellipsis(content, payloadEcho), manifest)
			case old != content:
				t.Errorf("txlog replay MODIFIED a file outside the workspace: %s (%q → %q)\n"+
					"manifest: %s", path, textfmt.Ellipsis(old, payloadEcho),
					textfmt.Ellipsis(content, payloadEcho), manifest)
			}
		}
	})
}

// confinedToRoot reports whether every op path in manifest resolves inside root.
// A manifest that does not decode has no paths to execute, so it is confined by
// definition and is allowed through — the decode path is part of what is fuzzed.
func confinedToRoot(manifest, root string) bool {
	var m struct {
		Ops []struct {
			Path string `json:"path"`
		} `json:"ops"`
	}
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		return true
	}
	for _, op := range m.Ops {
		if op.Path == "" {
			continue
		}
		if !filepath.IsAbs(op.Path) {
			return false // would resolve against the test process's working directory
		}
		rel, err := filepath.Rel(root, filepath.Clean(op.Path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

// treeSnapshot maps every regular file under dir to its content, so a replay's
// effect on that subtree can be compared exactly.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not what this test measures
		}
		b, readErr := os.ReadFile(path) //nolint:gosec // path comes from a walk of the test's own temp dir
		if readErr != nil {
			return nil
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
