package mcp

import (
	"bytes"
	"encoding/json"
	"testing"
)

// argguard_fuzz_test.go — the parameter-alias and argument-correction engine.
//
// TODO_IMPROVE_3 §9 names this parser as taking attacker-influenced input with
// no fuzz coverage. It does: tool arguments arrive from the MCP client, which is
// an agent whose instructions can be steered by any file it has read. The engine
// REWRITES those arguments before the tool sees them, which is what makes it
// worth fuzzing rather than merely testing — a rewrite is a chance to change
// meaning, not just spelling.
//
// WHAT IT FOUND, on its first run and then again nine seconds into the next.
//
// decodeArgsObject validated with a json.Decoder, which stops at the end of the
// first value and ignores whatever follows, while tools decode with plain
// json.Unmarshal, which refuses trailing data. So `{"file_path":"a"}garbage`
// passed the guard and then failed inside the tool with a bare JSON error rather
// than this layer's remediation-bearing one.
//
// The sharper half is that the outcome depended on something unrelated: when an
// alias rewrite fired, resolveArgs re-marshalled the decoded map and SILENTLY
// DROPPED the trailing bytes — so the identical input was accepted or rejected
// according to whether the caller happened to spell a parameter with an alias.
// Two parsers disagreeing about where the input ends is the classic shape of a
// smuggling bug, even where (as here) the stricter parser is the one downstream.
//
// The first fix was wrong in an instructive way: dec.More() reads as "is there
// more input?" but the standard library defines it as "the next byte is not ] or
// }", because it exists to iterate a container's elements. It therefore reported
// "nothing more" for `{"file_path":"a"} }`. The fix is a second Decode that must
// return io.EOF. Both payloads are retained as corpus.

// fuzzSchema is a stand-in for a write tool's contract: a couple of ordinary
// parameters plus the three shapes of safety flag plumb actually ships
// (dirty_ok, force, confirm). additionalProperties is absent, matching most
// real tool schemas, so the engine's extra-key handling is exercised rather
// than short-circuited by rejectExtra.
const fuzzSchema = `{
  "type": "object",
  "properties": {
    "file_path":   {"type": "string"},
    "content":     {"type": "string"},
    "old_string":  {"type": "string"},
    "line":        {"type": "integer"},
    "dirty_ok":    {"type": "boolean"},
    "force":       {"type": "boolean"},
    "confirm":     {"type": "boolean"},
    "edits":       {"type": "array", "items": {"type": "object", "properties": {
                      "old_string": {"type": "string"},
                      "new_string": {"type": "string"}}}}
  },
  "required": ["file_path"]
}`

// safetyFlags are the parameters whose value decides whether a guard applies.
// Synthesising one the caller never supplied is privilege escalation, not a
// spelling correction — and a correction engine that "helpfully" fills in a
// missing flag is exactly how that would happen.
var safetyFlags = []string{"dirty_ok", "force", "confirm"}

// FuzzResolveArgs asserts three properties of the rewrite, in order of how much
// they matter:
//
//  1. It never panics, whatever the client sends.
//  2. It never SYNTHESISES a safety flag. If the output carries dirty_ok, force
//     or confirm, the input must have carried something that could plausibly
//     have become it — concretely, the input must have had at least as many
//     keys, since every alias rewrite consumes the key it renames. A guard the
//     caller never asked to lift must not be lifted for them.
//  3. A successful rewrite still decodes as a JSON object. A tool receiving
//     bytes that no longer parse would fail in a much less legible place.
//
// The schema is fixed rather than fuzzed: schemas come from plumb's own registry
// and are not attacker-influenced, so fuzzing them would spend the budget on an
// input the threat model does not admit. parseShape's own fail-open contract is
// covered by TestParseShape_* in argguard_test.go.
func FuzzResolveArgs(f *testing.F) {
	// Well-formed, no aliasing needed.
	f.Add(`{"file_path":"/tmp/a.go","content":"package main"}`)
	// The canonical alias cases the engine exists for.
	f.Add(`{"path":"/tmp/a.go","content":"x"}`)
	f.Add(`{"filePath":"/tmp/a.go","text":"x"}`)
	f.Add(`{"uri":"file:///tmp/a.go"}`)
	// Safety flags supplied explicitly — these MUST survive untouched.
	f.Add(`{"file_path":"/a","dirty_ok":true}`)
	f.Add(`{"file_path":"/a","force":true,"confirm":true}`)
	// Near-misses on a safety flag. The engine DOES normalise case and separators,
	// so `dirtyok`, `dirty-ok` and `DirtyOk` all become `dirty_ok` — that is what
	// the alias engine is for, and an earlier version of this comment claimed the
	// opposite while nothing enforced either reading. What must hold is weaker and
	// checkable: a safety flag may only appear by CONSUMING an input key, never
	// from nothing. `dirty_okay` is far enough away to be left alone.
	f.Add(`{"file_path":"/a","dirtyok":true}`)
	f.Add(`{"file_path":"/a","dirty-ok":true}`)
	f.Add(`{"file_path":"/a","DirtyOk":true}`)
	f.Add(`{"file_path":"/a","dirty_okay":true}`)
	// Whitespace that Unicode calls space and JSON does not. Trimming these before
	// the trailing-byte check reopened the differential this target found.
	f.Add("{\"file_path\":\"/a\"}\v")
	f.Add("{\"file_path\":\"/a\"}\f")
	f.Add("{\"path\":\"/a\"}\v")
	f.Add("{\"file_path\":\"/a\"} ")
	// Nested and array-of-object levels, where the rewrite recurses.
	f.Add(`{"file_path":"/a","edits":[{"oldString":"a","newString":"b"}]}`)
	f.Add(`{"file_path":"/a","edits":[{},{},{}]}`)
	// Type-coercion surface: strings where scalars are declared.
	f.Add(`{"file_path":"/a","line":"12"}`)
	f.Add(`{"file_path":"/a","dirty_ok":"true"}`)
	f.Add(`{"file_path":"/a","dirty_ok":1}`)
	// Structurally hostile: duplicates, deep nesting, wrong root type, junk.
	f.Add(`{"file_path":"/a","file_path":"/b"}`)
	f.Add(`{"edits":[[[[[[[[[[]]]]]]]]]]}`)
	f.Add(`[]`)
	f.Add(`"a string"`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`{`)

	sh, ok := parseShape(json.RawMessage(fuzzSchema))
	if !ok {
		f.Fatal("fuzzSchema must parse — the harness is wrong, not the engine")
	}

	f.Fuzz(func(t *testing.T, args string) {
		raw := json.RawMessage(args)
		inKeys, inDecoded := topLevelKeys(raw)

		// Property 0: this layer and the tools agree on what the same bytes MEAN.
		//
		// The strongest oracle here, and the one that was missing: a differential
		// against the parser every tool actually uses. Whenever decodeArgsObject
		// accepts input json.Unmarshal rejects, the guard has silently sanitised
		// something on its way through — and if an alias also fired, the offending
		// bytes vanish from the re-marshalled output, so the SAME input succeeds or
		// fails depending on how the caller happened to spell a parameter.
		//
		// Checked FIRST, before the early return below: a divergence in the other
		// direction — the guard rejecting what the stdlib accepts — is a broken
		// valid call, and returning early on rejection would hide exactly that half.
		//
		// A property rather than a table because the first version of this fix
		// closed one divergence (trailing bytes) and opened another: bytes.TrimSpace
		// strips \v, \f, NBSP, NEL, LS and PS, none of which RFC 8259 permits. A
		// table only ever pins the cases someone thought of; this found the second
		// one in seconds.
		// One divergence is deliberate and documented: MCP allows `arguments` to be
		// absent, so the guard maps empty (or whitespace-only) input to an empty
		// object while json.Unmarshal calls it a syntax error. Exempted explicitly
		// rather than by weakening the comparison — this test found that case on its
		// first run, which is the property working, not failing.
		if len(bytes.Trim(raw, jsonWhitespace)) > 0 {
			var viaUnmarshal map[string]json.RawMessage
			unmarshalErr := json.Unmarshal(raw, &viaUnmarshal)
			if _, guardErr := decodeArgsObject(raw); (guardErr == nil) != (unmarshalErr == nil) {
				t.Fatalf("guard and json.Unmarshal disagree on the same bytes\nin:     %q\nguard:  %v\nstdlib: %v",
					args, guardErr, unmarshalErr)
			}
		}

		out, _, err := resolveArgs(sh, raw, "write_file")
		if err != nil {
			return // a rejected call never reaches a tool
		}

		outKeys, outDecoded := topLevelKeys(out)
		if !outDecoded {
			t.Fatalf("resolveArgs returned success but the result does not decode as an object\nin:  %s\nout: %s", args, out)
		}

		for _, flag := range safetyFlags {
			if _, present := outKeys[flag]; !present {
				continue
			}
			if _, wasThere := inKeys[flag]; wasThere {
				continue // supplied explicitly: correct to keep
			}
			// Not supplied under its canonical name, so it can only have arrived by
			// RENAMING an input key into it — which consumes that key. If the output
			// has at least as many keys as the input, nothing was consumed and the
			// flag came from nowhere.
			//
			// The earlier predicate was `!inDecoded || len(inKeys) == 0`, which the
			// trailing-byte fix made UNREACHABLE: the only inputs that both decoded
			// to no keys and survived resolveArgs were the ones now rejected. It
			// survived a mutation that set dirty_ok unconditionally on every call.
			if !inDecoded || len(outKeys) > len(inKeys) {
				t.Errorf("resolveArgs SYNTHESISED the safety flag %q with no input key to account for it\nin:  %s (%d keys)\nout: %s (%d keys)",
					flag, args, len(inKeys), out, len(outKeys))
			}
		}
	})
}

// topLevelKeys decodes raw as a JSON object and returns its top-level keys.
// ok is false when raw is not an object — every caller treats that as "carried
// no keys" rather than failing, because the engine is allowed to reject it.
func topLevelKeys(raw json.RawMessage) (keys map[string]struct{}, ok bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	keys = make(map[string]struct{}, len(obj))
	for k := range obj {
		keys[k] = struct{}{}
	}
	return keys, true
}
