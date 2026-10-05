package tools

// write_diff_gate_test.go — the sensitive-path gate at every write site, driven
// through the REAL matcher the daemon wires.
//
// The sibling tests use sensitiveTo(...), a stub keyed on exact path strings. A
// stub answers the one question it was built for and cannot fail for any other
// reason, which is exactly how a real defect hid here: the gate is handed a path
// and MatchSensitive resolves the globs against the WORKSPACE ROOT, so a site
// passing a path relative to something else (find_replace's search root) silently
// degraded every path-shaped glob to a base-name match. Only a test that runs the
// production rule can see that, so this file uses history.IsSensitiveChange (the
// one rule the store and the daemon's gate share) with a path-shaped glob and a
// real workspace root.
//
// Every case runs TWICE: once with a glob that matches, and once with a glob that
// cannot match, so a marker that appears for the wrong reason — or content that
// disappears for one — cannot pass as the gate working.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// secretValue is the content that must never reach a response for a withheld
// path. The PATH is allowed to appear; the value is not.
const secretValue = "SECRETVALUE"

// matchNothing cannot match any path shape IsSensitiveChange tries.
const matchNothing = "no-such-glob-*"

// realGate stands in for the daemon's gate with its rule: IsSensitiveChange over
// the change's path and source, one workspace root, and a glob list.
func realGate(root string, globs ...string) func(context.Context, string, string) bool {
	return func(_ context.Context, path, from string) bool {
		return history.IsSensitiveChange(globs, root, path, from)
	}
}

// siteCase is one write tool, run through its real Execute. The closures capture
// the subtest's *testing.T rather than taking it, so failures land on the case
// that produced them.
type siteCase struct {
	name    string
	prepare func(dir string) json.RawMessage
	run     func(deps WriteDeps, raw json.RawMessage) string
	// undo is the store the undo_edit case arms in prepare and reverts from in
	// run. It lives on the case because those are two separate Execute calls, and
	// one store has to reach both. nil for every other case.
	undo *UndoStore
}

// sensitiveFile is the path every case places its secret content at: two
// directories deep, so a glob can only match it through the RELATIVE path and
// never through the base name. That is what makes the case fail if a site hands
// the matcher the wrong path.
func sensitiveFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "secrets", "prod.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("token: "+secretValue+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// siteCases covers every write site that can run without a language server.
// move_symbol and the semantic symbol edits are absent for that reason; like the
// reviewer, they can only be checked by reading the code, which is why the shared
// helpers they call are covered by write_diff_response_test.go.
func siteCases(t *testing.T) []siteCase {
	t.Helper()
	// One store per call, shared by the undo_edit case's arming write and its
	// undo. A fresh one per call keeps the two parent tests independent.
	undoStore := NewUndoStore()
	return []siteCase{
		{
			name: "write_file",
			prepare: func(dir string) json.RawMessage {
				p := sensitiveFile(t, dir)
				return writeJSON(t, map[string]any{"file_path": p, "content": "token: " + secretValue + "\nsecond: line\n"})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewWriteFile(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("write_file: %v", err)
				}
				return out
			},
		},
		{
			name: "edit_file",
			prepare: func(dir string) json.RawMessage {
				p := sensitiveFile(t, dir)
				return writeJSON(t, map[string]any{
					"file_path": p,
					"edits":     []map[string]any{{"old_string": secretValue, "new_string": "ROTATED"}},
				})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewEditFile(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("edit_file: %v", err)
				}
				return out
			},
		},
		{
			name: "find_replace",
			prepare: func(dir string) json.RawMessage {
				sensitiveFile(t, dir)
				return writeJSON(t, map[string]any{
					"path": dir, "pattern": secretValue, "replacement": "ROTATED",
					"glob": "**/*.yaml", "dry_run": true,
				})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewFindReplace(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("find_replace: %v", err)
				}
				return out
			},
		},
		{
			name: "transaction_apply",
			prepare: func(dir string) json.RawMessage {
				p := sensitiveFile(t, dir)
				return writeJSON(t, map[string]any{"operations": []map[string]any{{
					"file_path": p,
					"edits":     []map[string]any{{"old_string": secretValue, "new_string": "ROTATED"}},
				}}})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewTransactionApply(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("transaction_apply: %v", err)
				}
				return out
			},
		},
		{
			name: "delete_file",
			prepare: func(dir string) json.RawMessage {
				return writeJSON(t, map[string]any{"file_path": sensitiveFile(t, dir)})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewDeleteFile(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("delete_file: %v", err)
				}
				return out
			},
		},
		{
			name: "copy_file",
			prepare: func(dir string) json.RawMessage {
				return writeJSON(t, map[string]any{
					"from": sensitiveFile(t, dir), "to": filepath.Join(dir, "public", "out.yaml"), "dirty_ok": true,
				})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewCopyFile(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("copy_file: %v", err)
				}
				return out
			},
		},
		{
			name: "rename_file",
			prepare: func(dir string) json.RawMessage {
				// The DESTINATION is the sensitive path: renaming over it is what
				// makes the response render its content.
				dst := sensitiveFile(t, dir)
				src := filepath.Join(dir, "public", "src.yaml")
				if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(src, []byte("ordinary: content\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return writeJSON(t, map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewRenameFile(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("rename_file: %v", err)
				}
				return out
			},
		},
		{
			name: "undo_edit",
			undo: undoStore,
			prepare: func(dir string) json.RawMessage {
				p := sensitiveFile(t, dir)
				// Arm the undo snapshot the way a real session would, so the undo
				// has something to restore. The arming write uses its own deps; the
				// store is the one carried on this case.
				if _, err := NewWriteFile(WriteDeps{Undo: undoStore}).Execute(context.Background(),
					writeJSON(t, map[string]any{"file_path": p, "content": "token: " + secretValue + "\nsecond: line\n"})); err != nil {
					t.Fatalf("arming write: %v", err)
				}
				return writeJSON(t, map[string]any{"file_path": p})
			},
			run: func(deps WriteDeps, raw json.RawMessage) string {
				out, err := NewUndoEdit(deps).Execute(context.Background(), raw)
				if err != nil {
					t.Fatalf("undo_edit: %v", err)
				}
				return out
			},
		},
	}
}

func TestEverySiteWithholdsASensitivePath(t *testing.T) {
	for _, tc := range siteCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			// Withheld: a path-shaped glob that matches this file only through
			// its workspace-relative path.
			dir := t.TempDir()
			raw := tc.prepare(dir)
			deps := WriteDeps{ShowWriteDiff: true, Undo: tc.undo, SensitivePathFn: realGate(dir, "secrets/*")}
			out := tc.run(deps, raw)
			if !strings.Contains(out, withheldSensitiveNote) {
				t.Fatalf("sensitive path rendered no withholding marker:\n%s", out)
			}
			if strings.Contains(out, secretValue) {
				t.Fatalf("the withheld path's content reached the response:\n%s", out)
			}
			if strings.Contains(out, relayDiffNote) {
				t.Fatalf("a withheld diff still asked the agent to relay it:\n%s", out)
			}
		})
	}
}

func TestEverySiteStillShowsANonSensitivePath(t *testing.T) {
	// The positive control for the test above: same fixtures, same code path, a
	// glob that cannot match. If the marker above were coming from a broken gate
	// rather than the policy, this fails.
	for _, tc := range siteCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			raw := tc.prepare(dir)
			deps := WriteDeps{ShowWriteDiff: true, Undo: tc.undo, SensitivePathFn: realGate(dir, matchNothing)}
			out := tc.run(deps, raw)
			if strings.Contains(out, withheldSensitiveNote) {
				t.Fatalf("an unmatched glob withheld the diff:\n%s", out)
			}
			if !strings.Contains(out, secretValue) {
				t.Fatalf("the content was not shown:\n%s", out)
			}
		})
	}
}
