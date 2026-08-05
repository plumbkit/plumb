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
//
// The rejection is pinned as a WHOLE STRING, not by substring. Substring
// assertions are what let the message stay false for a release: every case here
// is alias-vs-alias, so the canonical is in the object only because the rewrite
// put it there, and the message used to report it as something the caller
// "supplied" and should "keep" — a name that appears nowhere in their call.
func TestResolveArgs_AliasCollision_SecondKeyRejectedNotDropped(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		args    string
		wantWin string // the rewrite that must be applied
		wantErr string // the rejection, in full
	}{
		{
			name:    "text + body both mean content (write_file)",
			schema:  `{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"],"additionalProperties":false}`,
			args:    `{"file_path":"/f","body":"B","text":"A"}`,
			wantWin: `"content":"B"`, // "body" sorts before "text"
			wantErr: `tool: unknown parameter "text": you supplied both "body" and "text", ` +
				`which both name "content" here — remove one. Valid parameters: file_path, content`,
		},
		{
			name:    "path + file both mean file_path (read_file)",
			schema:  `{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"integer"}},"required":["file_path"],"additionalProperties":false}`,
			args:    `{"file":"/dir/a.txt","path":"/dir"}`,
			wantWin: `"file_path":"/dir/a.txt"`, // "file" sorts before "path"
			wantErr: `tool: unknown parameter "path": you supplied both "file" and "path", ` +
				`which both name "file_path" here — remove one. Valid parameters: file_path, limit`,
		},
		{
			name:    "filename + filepath both mean file_path",
			schema:  `{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"],"additionalProperties":false}`,
			args:    `{"filename":"/a","filepath":"/b"}`,
			wantWin: `"file_path":"/a"`,
			wantErr: `tool: unknown parameter "filepath": you supplied both "filename" and "filepath", ` +
				`which both name "file_path" here — remove one. Valid parameters: file_path`,
		},
		{
			name:    "dir + folder both mean path",
			schema:  `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
			args:    `{"dir":"/a","folder":"/b"}`,
			wantWin: `"path":"/a"`,
			wantErr: `tool: unknown parameter "folder": you supplied both "dir" and "folder", ` +
				`which both name "path" here — remove one. Valid parameters: path`,
		},
		{
			name:    "msg + commit_message both mean message (git)",
			schema:  `{"type":"object","properties":{"subcommand":{"type":"string"},"message":{"type":"string"}},"required":["subcommand"],"additionalProperties":false}`,
			args:    `{"subcommand":"commit","commit_message":"A","msg":"B"}`,
			wantWin: `"message":"A"`, // "commit_message" sorts before "msg"
			wantErr: `tool: unknown parameter "msg": you supplied both "commit_message" and "msg", ` +
				`which both name "message" here — remove one. Valid parameters: subcommand, message`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh, ok := parseShape(json.RawMessage(tc.schema))
			if !ok {
				t.Fatal("parseShape failed")
			}
			// Repeat: the losing key must be chosen by sort order, not by Go's
			// randomised map iteration, so the outcome is stable run to run —
			// and so is every name the message quotes.
			for range 20 {
				_, _, err := resolveArgs(sh, json.RawMessage(tc.args), "tool")
				if err == nil {
					t.Fatalf("colliding aliases accepted silently; one supplied value was dropped (args %s)", tc.args)
				}
				if got := err.Error(); got != tc.wantErr {
					t.Fatalf("rejection message\n got: %s\nwant: %s", got, tc.wantErr)
				}
			}
		})
	}
}

// TestUnknownErr_NamesTheCollisionAndSeparatesTheParameterList covers what the
// rejection above SAYS, which the tests around it only spot-check for the losing
// key's name.
//
// A both-supplied collision is not an ordinary unknown parameter, and reporting
// it as one is actively misleading: the tool understands the name, and the
// generic "did you mean" hint points at the parameter the caller has already
// supplied, which reads as nonsense ("unknown parameter \"root\"; did you mean
// \"path\"?" — they passed path). The caller cannot act on that. Naming both
// keys and saying which to drop is the whole fix.
//
// There are two truthful wordings, and picking the wrong one is its own bug.
// The rewrite runs BEFORE validation, so "the canonical is present" does not
// mean "the caller typed it": only when they really did can the message tell
// them to keep it. Each case is pinned as a whole string — a substring check is
// exactly what let the alias-vs-alias wording stay false.
//
// The separator is checked here too because it is the same sentence: the list
// used to be appended with a bare space, so a message ended "... unknown
// parameter \"root\" valid parameters: pattern, path, ..." — one run-on clause
// with no boundary between the complaint and the inventory.
func TestUnknownErr_NamesTheCollisionAndSeparatesTheParameterList(t *testing.T) {
	// find_files' real shape: `path` is canonical and `root` is the retired
	// list_files spelling that aliases onto it.
	const findFiles = `{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"max_depth":{"type":"integer"}},"additionalProperties":false}`

	// An alias whose canonical the caller REALLY typed. Only here is "keep the
	// other one" sound advice, because the other one is in their call.
	t.Run("canonical genuinely supplied names both keys and the one to drop", func(t *testing.T) {
		sh, ok := parseShape(json.RawMessage(findFiles))
		if !ok {
			t.Fatal("parseShape failed")
		}
		_, _, err := resolveArgs(sh, json.RawMessage(`{"path":"/a","root":"/b"}`), "find_files")
		if err == nil {
			t.Fatal("supplying both an alias and its canonical must be rejected, not silently resolved")
		}
		const want = `find_files: unknown parameter "root": you supplied both "root" and "path", ` +
			`which name the same parameter here — remove "root" and keep "path". ` +
			`Valid parameters: pattern, path, max_depth`
		if got := err.Error(); got != want {
			t.Errorf("rejection message\n got: %s\nwant: %s", got, want)
		}
		if strings.Contains(err.Error(), "did you mean") {
			t.Errorf(`a both-supplied collision must not fall back to the "did you mean" hint — it `+
				`would point at the parameter already supplied: %s`, err)
		}
	})

	// The alias-vs-alias case, at a level validation reaches only by recursing.
	// The canonical is quoted BARE (it is the level's own parameter name) while
	// the two supplied keys carry their full dotted paths, so a caller with a
	// long edits array can see which element the complaint is about.
	t.Run("nested: two aliases of one canonical name both supplied keys, not the canonical", func(t *testing.T) {
		const editFile = `{"type":"object","properties":{"file_path":{"type":"string"},` +
			`"edits":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"}},` +
			`"additionalProperties":false}}},"additionalProperties":false}`
		sh, ok := parseShape(json.RawMessage(editFile))
		if !ok {
			t.Fatal("parseShape failed")
		}
		_, _, err := resolveArgs(sh, json.RawMessage(`{"file_path":"/f","edits":[{"body":"B","text":"A"}]}`), "edit_file")
		if err == nil {
			t.Fatal("colliding aliases inside an array element must be rejected too")
		}
		const want = `edit_file: unknown parameter "edits[].text": you supplied both "edits[].body" ` +
			`and "edits[].text", which both name "content" here — remove one. Valid parameters: content`
		if got := err.Error(); got != want {
			t.Errorf("rejection message\n got: %s\nwant: %s", got, want)
		}
	})

	// The synthesised-canonical record is keyed per level: a canonical genuinely
	// supplied deep in the tree must still get the genuine wording, never be
	// mistaken for one the rewrite invented at another level.
	t.Run("nested: canonical genuinely supplied keeps the genuine wording", func(t *testing.T) {
		const editFile = `{"type":"object","properties":{"file_path":{"type":"string"},` +
			`"edits":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"}},` +
			`"additionalProperties":false}}},"additionalProperties":false}`
		sh, ok := parseShape(json.RawMessage(editFile))
		if !ok {
			t.Fatal("parseShape failed")
		}
		_, _, err := resolveArgs(sh, json.RawMessage(`{"file_path":"/f","edits":[{"content":"C","text":"A"}]}`), "edit_file")
		if err == nil {
			t.Fatal("an alias colliding with its genuinely-supplied canonical must be rejected")
		}
		const want = `edit_file: unknown parameter "edits[].text": you supplied both "edits[].text" ` +
			`and "content", which name the same parameter here — remove "edits[].text" and keep "content". ` +
			`Valid parameters: content`
		if got := err.Error(); got != want {
			t.Errorf("rejection message\n got: %s\nwant: %s", got, want)
		}
	})

	t.Run("a genuine unknown keeps the suggestion and is not called a collision", func(t *testing.T) {
		sh, ok := parseShape(json.RawMessage(findFiles))
		if !ok {
			t.Fatal("parseShape failed")
		}
		_, _, err := resolveArgs(sh, json.RawMessage(`{"maxdepht":2}`), "find_files")
		if err == nil {
			t.Fatal("an undeclared parameter must be rejected")
		}
		msg := err.Error()
		if strings.Contains(msg, "you supplied both") {
			t.Errorf("a key whose canonical was NOT supplied is not a collision: %s", msg)
		}
		if !strings.Contains(msg, `did you mean "max_depth"?`) {
			t.Errorf("a near-miss must keep its suggestion: %s", msg)
		}
	})

	t.Run("the parameter list is separated from the complaint", func(t *testing.T) {
		sh, ok := parseShape(json.RawMessage(findFiles))
		if !ok {
			t.Fatal("parseShape failed")
		}
		for _, args := range []string{
			`{"zzzzzzzzzz":1}`,       // no suggestion, no collision
			`{"maxdepht":2}`,         // suggestion
			`{"path":"/a","root":1}`, // collision
		} {
			_, _, err := resolveArgs(sh, json.RawMessage(args), "find_files")
			if err == nil {
				t.Fatalf("%s: want a rejection", args)
			}
			msg := err.Error()
			i := strings.Index(msg, "Valid parameters:")
			if i <= 0 {
				t.Fatalf("%s: message must end with the parameter list: %s", args, msg)
			}
			// Whatever the branch, the clause before the list must close: a stop
			// for a statement, a question mark for the "did you mean" hint.
			if before := strings.TrimSpace(msg[:i]); !strings.HasSuffix(before, ".") && !strings.HasSuffix(before, "?") {
				t.Errorf("%s: no separator before the parameter list — %q runs straight into it", args, before)
			}
		}
	})
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
