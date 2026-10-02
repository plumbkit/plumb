package cli

// serve_resume_strip_test.go — the resume credential never reaches the client.
//
// Claude Code persists a tool result's `_meta` in its on-disk transcripts, where a model
// with file tools can read it. The daemon discloses the credential only to a proxy that
// announced it strips the key, so these tests are the other half of that promise: every
// kind of frame the daemon can send goes through the real proxy and the client's view of
// it is scanned. A scan that has never seen a credential proves nothing by finding none,
// so every test has its positive control: the credential was captured for the store.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

const rcKey = mcp.MetaResumeCredentialKey

// withoutKey is v with the member named rcKey removed from every `_meta` object, at any
// depth: what a correct strip must leave behind. A member of that name anywhere else is
// data, and stays.
func withoutKey(v any) any { return withoutKeyIn(v, false) }

func withoutKeyIn(v any, inMeta bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if k == rcKey && inMeta {
				continue
			}
			out[k] = withoutKeyIn(val, k == "_meta")
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = withoutKeyIn(val, false)
		}
		return out
	}
	return v
}

func TestStripResumeCredential_EveryFrameShape(t *testing.T) {
	t.Parallel()
	secret := rcSecret(7)
	member := `"` + rcKey + `":"` + secret + `"`
	cases := map[string]string{
		"an initialize result":              `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","_meta":{"dev.plumbkit/session-identity":{"recovery":"established"},` + member + `}}}`,
		"a tool result":                     `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"ok"}],"_meta":{` + member + `}}}`,
		"a late (C3) disclosure":            `{"jsonrpc":"2.0","id":4,"result":{"content":[{"type":"text","text":"ok"}],"isError":false,"_meta":{"dev.plumbkit/pin-scope":"agent",` + member + `}}}`,
		"a content item's _meta":            `{"jsonrpc":"2.0","id":5,"result":{"content":[{"type":"text","text":"ok","_meta":{` + member + `}}]}}`,
		"structuredContent":                 `{"jsonrpc":"2.0","id":6,"result":{"structuredContent":{"deep":[{"_meta":{` + member + `}}]}}}`,
		"a notification":                    `{"jsonrpc":"2.0","method":"notifications/message","params":{"_meta":{` + member + `}}}`,
		"a JSON-RPC batch":                  `[{"jsonrpc":"2.0","id":7,"result":{"_meta":{` + member + `}}},{"jsonrpc":"2.0","id":8,"result":{}}]`,
		"an error response":                 `{"jsonrpc":"2.0","id":9,"error":{"code":-32000,"message":"x","data":{"_meta":{` + member + `}}}}`,
		"_meta nested under _meta":          `{"jsonrpc":"2.0","id":10,"result":{"_meta":{"x":{"_meta":{` + member + `}}}}}`,
		"an escaped spelling of the key":    `{"jsonrpc":"2.0","id":11,"result":{"_meta":{"dev.plumbkit/resume-credential":"` + secret + `"}}}`,
		"whitespace between tokens":         "{ \"jsonrpc\" : \"2.0\", \"id\":12, \"result\" : { \"_meta\" : { \"" + rcKey + "\" :\n \"" + secret + "\" } } }",
		"HTML characters elsewhere in text": `{"jsonrpc":"2.0","id":13,"result":{"content":[{"type":"text","text":"a < b && c > d"}],"_meta":{` + member + `}}}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clean, got := scrubResumeCredential([]byte(frame))

			if strings.Contains(string(clean), secret) || leakShape.Match(clean) {
				t.Fatalf("the credential survived the strip:\n%s", clean)
			}
			var before, after any
			if err := json.Unmarshal([]byte(frame), &before); err != nil {
				t.Fatalf("fixture is not JSON: %v", err)
			}
			if err := json.Unmarshal(clean, &after); err != nil {
				t.Fatalf("the stripped frame is not JSON: %v\n%s", err, clean)
			}
			if !reflect.DeepEqual(withoutKey(before), after) {
				t.Errorf("the strip changed more than the key:\n before: %s\n after:  %s", frame, clean)
			}
			// Positive control: it was seen and kept for the store, not merely dropped.
			if len(got) != 1 || got[0] != secret {
				t.Errorf("captured %v, want exactly the disclosed credential", got)
			}
		})
	}
}

// Fail closed: a frame the structured pass cannot trust still never carries a token.
func TestStripResumeCredential_FailsClosed(t *testing.T) {
	t.Parallel()
	secret := rcSecret(8)
	cases := map[string]string{
		"a duplicate member a map would collapse": `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"` + rcKey + `":"` + secret + `"},"_meta":{}}}`,
		"a frame that is not JSON":                `{"result":{"_meta":{"` + rcKey + `":"` + secret,
		"a truncated frame":                       `{"jsonrpc":"2.0","id":2,"result":{"_meta":{"` + rcKey + `":"` + secret + `"}`,
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clean := stripResumeCredential([]byte(frame))
			if strings.Contains(string(clean), secret) {
				t.Fatalf("the credential survived:\n%s", clean)
			}
		})
	}
}

func TestStripResumeCredential_LeavesOtherFramesByteForByte(t *testing.T) {
	t.Parallel()
	for name, frame := range map[string]string{
		"an ordinary tool result": `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"a < b"}]}}`,
		"the identity snapshot":   `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"dev.plumbkit/session-identity":{"recovery":"restored"}}}}`,
		// The key's name in a text value is prose, not a member; and a token in text is the
		// daemon's own business (it never writes one there), not this strip's.
		"the key's name in text":    `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"the ` + rcKey + ` key"}]}}`,
		"a token-shaped text value": `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + rcSecret(9) + `"}]}}`,
		"the consumer announcement": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"` + mcp.MetaResumeCredentialConsumerKey + `":1}}}`,
		"a frame that is not JSON":  `not json`,
		"an empty frame":            ``,
		"a non-string credential":   `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`,
		"another dev.plumbkit key":  `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"dev.plumbkit/resume":"x"}}}`,
		// Only a `_meta` object carries the credential. A member of that name elsewhere is
		// the daemon relaying somebody's data, and the strip does not rewrite data.
		"the key outside any _meta":  `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"` + rcKey + `":"x"}}}`,
		"a result with only content": `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`,
	} {
		if got := stripResumeCredential([]byte(frame)); !bytes.Equal(got, []byte(frame)) {
			t.Errorf("%s was rewritten:\n before: %s\n after:  %s", name, frame, got)
		}
	}
}

// N1. Output that merely mentions the key, or holds something shaped like a credential
// (a file, a diff, a test fixture), is ordinary tool text. The strip must not redact it,
// whether or not the frame also carries a real disclosure in its `_meta`.
func TestStripResumeCredential_LeavesTextThatNamesTheKeyAlone(t *testing.T) {
	t.Parallel()
	text := "// the proxy strips " + rcKey + " from every frame\nconst fixture = \"" + rcSecret(21) + "\"\nvar again = `" + rcSecret(22) + "`"
	frame := func(meta string) string {
		res := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 5, "result": res})
		if err != nil {
			t.Fatal(err)
		}
		if meta == "" {
			return string(b)
		}
		return strings.Replace(string(b), `"result":{`, `"result":{"_meta":{`+meta+`},`, 1)
	}

	t.Run("a frame with no disclosure comes through byte for byte", func(t *testing.T) {
		t.Parallel()
		f := frame("")
		if got, secrets := scrubResumeCredential([]byte(f)); !bytes.Equal(got, []byte(f)) || len(secrets) != 0 {
			t.Fatalf("ordinary output was rewritten (captured %v):\n before: %s\n after:  %s", secrets, f, got)
		}
	})
	t.Run("a real disclosure is removed and the text beside it is not touched", func(t *testing.T) {
		t.Parallel()
		f := frame(`"` + rcKey + `":"` + rcSecret(23) + `"`)
		got, secrets := scrubResumeCredential([]byte(f))
		if len(secrets) != 1 || secrets[0] != rcSecret(23) {
			t.Fatalf("captured %v, want the disclosed credential", secrets)
		}
		var res struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				Meta    map[string]any          `json:"_meta"`
			} `json:"result"`
		}
		if err := json.Unmarshal(got, &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Result.Content) != 1 || res.Result.Content[0].Text != text {
			t.Errorf("the strip changed the tool text:\n want: %q\n got:  %q", text, res.Result.Content)
		}
		if _, still := res.Result.Meta[rcKey]; still || strings.Contains(string(got), rcSecret(23)) {
			t.Errorf("the credential survived: %s", got)
		}
	})
	t.Run("through the proxy, a tool result naming the key arrives unchanged", func(t *testing.T) {
		t.Parallel()
		_, p := newScriptedDaemon(func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
			if e.Method == "initialize" {
				return [][]byte{rcInitResult(t, e.ID, "established", rcSecret(1))}, false
			}
			if e.isRequest() {
				return [][]byte{rcToolResult(t, e.ID, text, "")}, false
			}
			return nil, false
		})
		srv := startRCServe(t, rcStore(t, t.TempDir(), "scope-n1"), p)
		var res struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(srv.call("read_file", map[string]any{"file_path": "internal/redact/redact_test.go"}, nil)), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Result.Content) != 1 || res.Result.Content[0].Text != text {
			t.Fatalf("the proxy changed what read_file returned:\n want: %q\n got:  %q", text, res.Result.Content)
		}
		if strings.Contains(srv.everything(), "[REDACTED:resume-credential]") {
			t.Error("the strip redacted ordinary output")
		}
	})
}

// A value that is not a credential is removed all the same, and never stored.
func TestScrubResumeCredential_KeepsOnlyCredentialShapedValues(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"a number":           `7`,
		"an object":          `{"x":"` + rcSecret(1) + `"}`,
		"a short string":     `"rsk1-short"`,
		"a foreign string":   `"hunter2"`,
		"an array":           `["` + rcSecret(2) + `"]`,
		"a null":             `null`,
		"a boolean":          `true`,
		"a wrong-prefix tok": `"rsk2-0000000000000000000003"`,
	} {
		frame := `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"` + rcKey + `":` + value + `}}}`
		clean, got := scrubResumeCredential([]byte(frame))
		if strings.Contains(string(clean), rcKey) {
			t.Errorf("%s: the key survived: %s", name, clean)
		}
		if len(got) != 0 {
			t.Errorf("%s: captured %v; only a credential-shaped string may be stored", name, got)
		}
	}
}

// TestServeProxy_NeverForwardsTheResumeCredential is the through-the-real-proxy version:
// the daemon discloses on every path the design names (the initialize result, a tool
// result, a late C3 disclosure, a notification, and a replayed handshake after a daemon
// restart, both its swallowed result and a notification that is forwarded), and the
// client sees none of it. The store is the positive control: every credential the daemon
// sent was captured, in order, so a strip that worked by never reading the frame fails.
func TestServeProxy_NeverForwardsTheResumeCredential(t *testing.T) {
	t.Parallel()
	const conv = "conv-strip"
	s := []string{"", rcSecret(1), rcSecret(2), rcSecret(3), rcSecret(4), rcSecret(5), rcSecret(6)}
	notify := func(secret string) []byte {
		return rcFrame(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/message", "params": map[string]any{"_meta": map[string]any{rcKey: secret}}})
	}

	// Daemon one answers by tool name.
	_, p1 := newScriptedDaemon(func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
		if e.Method == "initialize" {
			return [][]byte{rcInitResult(t, e.ID, "established", s[1])}, false
		}
		if !e.isRequest() {
			return nil, false
		}
		switch toolName(frame) {
		case "session_start": // a tool result carrying the successor
			return [][]byte{rcToolResult(t, e.ID, "oriented", s[2])}, false
		case "late": // a late (C3) disclosure, with a notification first
			return [][]byte{notify(s[3]), rcToolResult(t, e.ID, "late", s[3])}, false
		case "crash":
			return nil, true
		}
		return [][]byte{rcToolResult(t, e.ID, "ok", "")}, false
	})
	// Daemon two is the restarted daemon the proxy replays its handshake to: a
	// notification mid-handshake (forwarded) and the initialize result (swallowed).
	_, p2 := newScriptedDaemon(func(e rpcEnvelope, _ []byte) ([][]byte, bool) {
		switch {
		case e.Method == "initialize":
			return [][]byte{notify(s[4]), rcInitResult(t, e.ID, "restored", s[5])}, false
		case e.isRequest():
			return [][]byte{rcToolResult(t, e.ID, "after", s[6])}, false
		}
		return nil, false
	})
	store := rcStore(t, t.TempDir(), "scope-strip")
	srv := startRCServe(t, store, p1, p2)

	if got := srv.call("session_start", rcHooked(conv, nil), nil); !strings.Contains(got, "oriented") {
		t.Fatalf("the session_start result lost its content: %s", got)
	}
	if got := srv.call("late", nil, nil); !strings.Contains(got, "late") {
		t.Fatalf("the late-disclosure result lost its content: %s", got)
	}
	srv.call("crash", nil, nil) // the daemon dies; the proxy reconnects and replays the handshake
	if got := srv.call("anything", nil, nil); !strings.Contains(got, "after") {
		t.Fatalf("the proxy did not come back: %s", got)
	}
	srv.drain(200_000_000)

	// What the client may keep: the rest of the identity snapshot rides the same _meta.
	if !strings.Contains(srv.everything(), "dev.plumbkit/session-identity") {
		t.Error("the strip removed the identity snapshot as well; it must take only the credential")
	}
	srv.assertClientNeverSaw(s[1:]...)

	// Positive control. Every disclosure after the first link was captured, in order:
	// s2 (the link), s3 (late), s4 (a forwarded notification), s5 (a swallowed replayed
	// initialize result), s6 (a tool result on the restarted daemon).
	e, ok := store.load(conv)
	if !ok || e.Secret != s[6] || e.Generation != 5 {
		t.Fatalf("store holds %+v (found %v); want the last credential, s6, at generation 5 — "+
			"the proxy dropped a disclosure instead of keeping it", e, ok)
	}
}
