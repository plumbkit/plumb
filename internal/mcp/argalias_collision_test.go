package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two supplied aliases of ONE canonical must never both rewrite to it. Before
// the claimed-target guard, eligible() only checked that the canonical was
// absent from the input, so both keys resolved, both were deleted, and the
// alphabetically-later source silently won — losing one of two values the caller
// explicitly supplied. write_file({text, body}) wrote the wrong content;
// read_file/write_file({path, file}) operated on the wrong path.
//
// The correct outcome is: the first claimant (sorted, so it is deterministic)
// wins, and the loser falls through to validation's explicit rejection.
func TestResolveArgs_AliasCollision_SecondKeyRejectedNotDropped(t *testing.T) {
	tests := []struct {
		name      string
		schema    string
		args      string
		wantWin   string // the rewrite that must be applied
		wantInErr string // the key validation must reject
	}{
		{
			name:      "text + body both mean content (write_file)",
			schema:    `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"],"additionalProperties":false}`,
			args:      `{"file_path":"/f","body":"B","text":"A"}`,
			wantWin:   `"content":"B"`, // "body" sorts before "text"
			wantInErr: `"text"`,
		},
		{
			name:      "path + file both mean file_path (read_file)",
			schema:    `{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}`,
			args:      `{"file":"/dir/a.txt","path":"/dir"}`,
			wantWin:   `"file_path":"/dir/a.txt"`, // "file" sorts before "path"
			wantInErr: `"path"`,
		},
		{
			name:      "filename + filepath both mean file_path",
			schema:    `{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"],"additionalProperties":false}`,
			args:      `{"filename":"/a","filepath":"/b"}`,
			wantWin:   `"file_path":"/a"`,
			wantInErr: `"filepath"`,
		},
		{
			name:      "dir + folder both mean path",
			schema:    `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
			args:      `{"dir":"/a","folder":"/b"}`,
			wantWin:   `"path":"/a"`,
			wantInErr: `"folder"`,
		},
		{
			name:      "msg + commit_message both mean message (git)",
			schema:    `{"type":"object","properties":{"subcommand":{"type":"string"},"message":{"type":"string"}},"required":["subcommand"],"additionalProperties":false}`,
			args:      `{"subcommand":"commit","commit_message":"A","msg":"B"}`,
			wantWin:   `"message":"A"`, // "commit_message" sorts before "msg"
			wantInErr: `"msg"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh, ok := parseShape(json.RawMessage(tc.schema))
			if !ok {
				t.Fatal("parseShape failed")
			}
			// Repeat: the losing key must be chosen by sort order, not by Go's
			// randomised map iteration, so the outcome is stable run to run.
			for range 20 {
				_, _, err := resolveArgs(sh, json.RawMessage(tc.args), "tool")
				if err == nil {
					t.Fatalf("colliding aliases accepted silently; one supplied value was dropped (args %s)", tc.args)
				}
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Fatalf("error must reject the losing key %s, got: %v", tc.wantInErr, err)
				}
			}
		})
	}
}

// The same collision on a level that TOLERATES extras: validation cannot reject
// the loser there, so the guard's job is simply to leave it alone rather than
// let it overwrite the winner's value.
func TestResolveArgs_AliasCollision_ExtrasTolerated_FirstClaimantWins(t *testing.T) {
	sh, ok := parseShape(json.RawMessage(
		`{"type":"object","properties":{"content":{"type":"string"}},"additionalProperties":true}`))
	if !ok {
		t.Fatal("parseShape failed")
	}
	out, warnings, err := resolveArgs(sh, json.RawMessage(`{"body":"B","text":"A"}`), "tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"content":"B"`) {
		t.Errorf(`first claimant "body" must win: %s`, got)
	}
	if !strings.Contains(got, `"text":"A"`) {
		t.Errorf(`the losing key's value must be preserved, not dropped: %s`, got)
	}
	if n := len(warnings); n != 1 {
		t.Errorf("want exactly one rewrite warning, got %d: %v", n, warnings)
	}
}
