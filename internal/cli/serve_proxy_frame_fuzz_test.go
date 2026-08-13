package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// serve_proxy_frame_fuzz_test.go — the MCP framing layer of the resilient serve
// proxy.
//
// TODO_IMPROVE_3 §9 names MCP framing as a parser taking attacker-influenced
// input with no fuzz coverage. It does, and it is worse than "parses": the proxy
// REWRITES frames in flight. It folds `_meta` into the client's initialize
// request — including the allow-dirs grant, which widens the daemon's filesystem
// boundary — and it appends a note to tool results after a reconnect. A rewrite
// is a chance to change meaning, not just spelling, which is what makes this
// worth fuzzing rather than merely testing.
//
// Every property below is a DIFFERENTIAL or a round-trip against a second
// opinion, not "did not panic". That choice is the lesson of #258: the two
// panic/shape properties it shipped with found nothing, and the one property
// that compared the guard against json.Unmarshal found a real smuggling bug in
// seconds.
//
// The oracles:
//
//  1. ENVELOPE PRESERVATION. The proxy decides what a frame IS (parseEnvelope's
//     method and id) and then rewrites it. If the rewrite can change either, the
//     daemon acts on a different message than the proxy routed — the shape of
//     every request-smuggling bug.
//  2. INJECTION AUTHORITY. When the proxy holds an allow-dirs grant, the frame
//     the daemon receives must carry the PROXY's dirs, whatever the client wrote
//     in its own _meta. The grant comes from `serve --allow-dir`, so a client
//     that could keep its own value would be granting itself roots.
//  3. IDEMPOTENCE. replayHandshake replays the captured initialize frame on every
//     reconnect, and the capture is post-injection. Re-injecting must be stable,
//     or a long-lived session's handshake drifts with each reconnect.
//  4. APPEND-ONLY. injectReconnectNote's doc says it "only *appends* a content
//     item, never edits existing text". A document that vouches for a defence is
//     a testable claim — this tests it.
//  5. ID INJECTIVITY. idKey is the ROUTING KEY for in-flight requests
//     (trackOutstanding / resolveResponse). Two distinct request ids collapsing
//     to one key means a response delivered against the wrong request.

// proxyDirs is the grant a `plumb serve --allow-dir` session holds. Distinctive
// so a client-supplied value cannot be mistaken for it.
var proxyDirs = []string{"/granted/rw-root", "/granted/second"}

// clientForgedDirs is what a hostile client would like the daemon to believe it
// was granted.
const clientForgedDirs = `["/","/Users","/etc"]`

func FuzzProxyFrameRewrite(f *testing.F) {
	// Ordinary initialize requests.
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)
	f.Add(`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{}}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	// The client already supplies _meta — the injection must win on its own keys
	// and preserve the rest.
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"other":"keep"}}}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"` + mcp.MetaAllowDirsKey + `":` + clientForgedDirs + `}}}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"` + mcp.MetaWorkspaceKey + `":"/attacker/ws"}}}`)
	// Duplicate keys: Go's decoder takes the last, and a re-marshal emits one.
	// If the proxy's view and the daemon's could ever differ here, that is a
	// smuggle.
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"initialize","params":{}}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"id":2,"method":"initialize","params":{}}`)
	// Shapes the injector must refuse to touch.
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":[]}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":"scalar"}`)
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":42}}`)
	f.Add(`[1,2,3]`)
	f.Add(`"just a string"`)
	f.Add(``)
	f.Add(`{`)
	// tools/call results, for the reconnect-note appender.
	f.Add(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"hello"}]}}`)
	f.Add(`{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`)
	f.Add(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"a"},{"type":"image","data":"b"}],"isError":true}}`)
	f.Add(`{"jsonrpc":"2.0","id":7,"error":{"code":-32601,"message":"nope"}}`)
	f.Add(`{"jsonrpc":"2.0","id":7,"result":{}}`)
	// Unicode and escaping in text the appender must not disturb.
	f.Add(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"\u0000\ud83d\ude00 </script>"}]}}`)

	f.Fuzz(func(t *testing.T, frame string) {
		in := []byte(frame)
		kv := buildInitMeta(proxyDirs, "proxy-session-1", "/proxy/cwd")

		out := injectInitMeta(in, kv)
		checkEnvelopePreserved(t, in, out)
		checkInjectionAuthoritative(t, in, out)
		checkInjectionIdempotent(t, out, kv)

		noted, ok := injectReconnectNote(in, "0.16.6", "0.16.5", true)
		checkEnvelopePreserved(t, in, noted)
		if ok {
			checkAppendOnly(t, in, noted)
		} else if !bytes.Equal(in, noted) {
			t.Errorf("injectReconnectNote reported ok=false but changed the frame\n in: %q\nout: %q", in, noted)
		}
	})
}

// checkEnvelopePreserved is oracle 1. It compares the proxy's OWN view of the
// frame before and after the rewrite, using the same parseEnvelope the routing
// code uses — so it asks the question the router asks, not a re-implementation
// of it.
func checkEnvelopePreserved(t *testing.T, in, out []byte) {
	t.Helper()
	if bytes.Equal(in, out) {
		return // untouched: nothing to preserve
	}
	before, after := parseEnvelope(in), parseEnvelope(out)
	if before.Method != after.Method {
		t.Errorf("rewrite changed the method: %q -> %q\n in: %q\nout: %q",
			before.Method, after.Method, in, out)
	}
	if got, want := idKey(after.ID), idKey(before.ID); got != want {
		t.Errorf("rewrite changed the request id: %q -> %q\n in: %q\nout: %q", want, got, in, out)
	}
	if before.isRequest() != after.isRequest() || before.isResponse() != after.isResponse() {
		t.Errorf("rewrite changed the message CLASS (request=%v->%v response=%v->%v)\n in: %q\nout: %q",
			before.isRequest(), after.isRequest(), before.isResponse(), after.isResponse(), in, out)
	}
}

// checkInjectionAuthoritative is oracle 2, the security-relevant one. If the
// rewrite happened at all, the allow-dirs the daemon will read must be the
// PROXY's grant — never a value the client supplied.
func checkInjectionAuthoritative(t *testing.T, in, out []byte) {
	t.Helper()
	if bytes.Equal(in, out) {
		// The injector declined — the frame is not an object, its params are not an
		// object, or the rewrite would have moved the envelope. What a DECLINED
		// frame does with a client-supplied grant is a separate question with a
		// deliberately-accepted answer; see TestInjectInitMeta_DeclinedFrameKeepsClientMeta.
		return
	}
	got, ok := metaValue(out, mcp.MetaAllowDirsKey)
	if !ok {
		t.Errorf("injection happened but the allow-dirs grant is absent from _meta\nout: %q", out)
		return
	}
	var dirs []string
	if err := json.Unmarshal(got, &dirs); err != nil {
		t.Errorf("allow-dirs in _meta is not a string array: %s (%v)", got, err)
		return
	}
	if len(dirs) != len(proxyDirs) {
		t.Errorf("client influenced the allow-dirs grant: got %v, want the proxy's %v\n in: %q\nout: %q",
			dirs, proxyDirs, in, out)
		return
	}
	for i := range dirs {
		if dirs[i] != proxyDirs[i] {
			t.Errorf("client influenced the allow-dirs grant: got %v, want the proxy's %v\n in: %q\nout: %q",
				dirs, proxyDirs, in, out)
			return
		}
	}
}

// checkInjectionIdempotent is oracle 3. The captured (already-injected) frame is
// replayed and re-injected on every reconnect, so injecting twice must equal
// injecting once.
func checkInjectionIdempotent(t *testing.T, once []byte, kv map[string]json.RawMessage) {
	t.Helper()
	twice := injectInitMeta(once, kv)
	if !bytes.Equal(once, twice) {
		t.Errorf("injection is not idempotent, so a replayed handshake drifts\nonce:  %q\ntwice: %q", once, twice)
	}
}

// checkAppendOnly is oracle 4: injectReconnectNote's documented contract. Every
// original content item must survive, unchanged, in order, as a PREFIX of the
// new array — the note may only be added after them.
func checkAppendOnly(t *testing.T, in, out []byte) {
	t.Helper()
	before, okBefore := resultContent(in)
	after, okAfter := resultContent(out)
	if !okBefore || !okAfter {
		t.Errorf("injectReconnectNote reported success but the result.content shape did not survive\n in: %q\nout: %q", in, out)
		return
	}
	if len(after) != len(before)+1 {
		t.Errorf("append-only violated: %d content items became %d, want %d\n in: %q\nout: %q",
			len(before), len(after), len(before)+1, in, out)
		return
	}
	for i := range before {
		if !jsonEquivalent(before[i], after[i]) {
			t.Errorf("append-only violated: content item %d was rewritten\nbefore: %s\nafter:  %s", i, before[i], after[i])
		}
	}
}

// metaValue pulls params._meta[key] out of a frame.
func metaValue(frame []byte, key string) (json.RawMessage, bool) {
	var full map[string]json.RawMessage
	if json.Unmarshal(frame, &full) != nil {
		return nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(full["params"], &params) != nil {
		return nil, false
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil {
		return nil, false
	}
	v, ok := meta[key]
	return v, ok
}

// resultContent pulls result.content out of a frame as raw items.
func resultContent(frame []byte) ([]json.RawMessage, bool) {
	var full map[string]json.RawMessage
	if json.Unmarshal(frame, &full) != nil {
		return nil, false
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(full["result"], &result) != nil {
		return nil, false
	}
	var content []json.RawMessage
	if json.Unmarshal(result["content"], &content) != nil {
		return nil, false
	}
	return content, true
}

// jsonEquivalent compares two raw JSON values by their decoded meaning, so a
// re-marshal that only reorders object keys or rewrites an escape is not
// reported as an edit. The claim under test is "never edits existing text", and
// re-encoding is not editing.
func jsonEquivalent(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(a, b)
	}
	ab, aerr := json.Marshal(av)
	bb, berr := json.Marshal(bv)
	if aerr != nil || berr != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ab, bb)
}

// FuzzIDKeyRoutesResponsesToRequests is oracle 5, and it has its own target
// because it is an END-TO-END property rather than a property of idKey alone.
//
// idKey is the routing key for in-flight requests: trackOutstanding stores under
// idKey(request.id), resolveResponse deletes under idKey(response.id). What has
// to hold is therefore not that idKey is injective, but that a request and the
// response to it produce the SAME key after the id has made a round trip through
// the daemon.
//
// That distinction is not pedantry — it is the whole finding. The daemon carries
// its id as `ID any` (internal/mcp/server.go), so json.Unmarshal gives float64
// and the daemon re-marshals THAT into the response. Any precision the client's
// id had beyond float64 is destroyed before the proxy ever sees the reply. The
// proxy's own float64 normalisation is what keeps the two sides agreeing — it is
// compensating for the daemon, not merely tidying whitespace.
//
// The first version of this target asserted injectivity instead, and it fired
// immediately on its own seeds: ids 10000000000000000001 and ...002 share a key.
// That IS a real defect (two in-flight requests collide, one response resolves
// the wrong request), but the obvious fix — decode with UseNumber so digits
// survive — was measured to be a REGRESSION. It makes the request key stop
// matching the daemon's echoed response key, so the response becomes unroutable
// and the request is error-synthesised on the next reconnect: a second response
// for an already-answered id, which is worse than the collision.
//
// The collision cannot be fixed here. It has to be fixed in internal/mcp, by
// carrying the id as json.RawMessage so the daemon echoes bytes rather than a
// float. Until then this target guards the property that actually protects
// routing today, and would have caught that bad fix. The collision payloads stay
// in the seed corpus so they are exercised rather than forgotten.
func FuzzIDKeyRoutesResponsesToRequests(f *testing.F) {
	f.Add(`1`)
	f.Add(`42`)
	f.Add(`1.0`)
	f.Add(` 1 `)
	f.Add(`"x"`)
	f.Add(`"a-uuid-4f3b"`)
	f.Add(`null`)
	// Beyond float64's exact integer range. These are the payloads that made the
	// injectivity version fail and the naive fix regress; they must keep routing.
	f.Add(`10000000000000000001`)
	f.Add(`10000000000000000002`)
	f.Add(`9007199254740993`)
	f.Add(`-10000000000000000001`)
	f.Add(`1e400`)
	f.Add(`0.1`)

	f.Fuzz(func(t *testing.T, id string) {
		raw := json.RawMessage(id)
		if !json.Valid(raw) {
			t.Skip() // the proxy only reaches idKey for ids that parsed as JSON
		}
		echoed, ok := daemonEchoedID(raw)
		if !ok {
			t.Skip() // the daemon could not carry this id at all
		}
		if got, want := idKey(echoed), idKey(raw); got != want {
			t.Errorf("a response to request id %s would not route back to it:\n"+
				"  request key:  %q\n  response key: %q\n  daemon echoed: %s\n"+
				"outstanding is keyed on this string, so the request is never resolved "+
				"and gets error-synthesised on the next reconnect", id, want, got, echoed)
		}
	})
}

// daemonEchoedID models what the daemon does to a request id on its way into a
// response: internal/mcp carries it as `ID any`, so it is decoded into an
// interface and re-marshalled. Modelled rather than invoked because the property
// under test is about the ENCODING the proxy sees, and reproducing it here keeps
// the target free of a live server.
//
// If internal/mcp ever changes to json.RawMessage — which is the correct fix for
// the collision described above — this model must change with it, and the
// mismatch will show up as this target failing rather than as silent drift.
func daemonEchoedID(raw json.RawMessage) (json.RawMessage, bool) {
	var carried any
	if err := json.Unmarshal(raw, &carried); err != nil {
		return nil, false
	}
	out, err := json.Marshal(carried)
	if err != nil {
		return nil, false
	}
	return out, true
}

// TestInjectInitMeta_DeclinedFrameKeepsClientMeta pins the LIMIT of the
// injection-authority property, so it is a payload-bearing fact rather than a
// sentence in a merged pull request nobody greps.
//
// injectInitMeta is authoritative only when it INJECTS. When it declines — an
// empty kv (the proxy holds no `serve --allow-dir` grant), a frame that is not a
// JSON object, or params that are not an object — the frame is returned byte for
// byte, so a client-supplied dev.plumbkit/allow-dirs in its own _meta reaches the
// daemon untouched.
//
// That is deliberate, not an oversight. The daemon trusts its socket peer: a
// client connecting directly, with no `plumb serve` in between, sets _meta itself
// and there is nobody to overrule it. The proxy overwriting the key when it has
// something to say, and passing the frame through when it does not, is the same
// contract. What would make it a defect is the proxy holding a grant and letting
// the client's value win, which is what the fuzz oracle covers.
//
// If the trust model ever changes so that a client may not name its own roots,
// this test is where that decision lands: it fails, and the fix is to strip the
// key on decline rather than to adjust the expectation.
func TestInjectInitMeta_DeclinedFrameKeepsClientMeta(t *testing.T) {
	t.Parallel()
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"` +
		mcp.MetaAllowDirsKey + `":["/","/etc"]}}}`)

	// No grant: buildInitMeta returns nil, so the injector declines.
	out := injectInitMeta(frame, buildInitMeta(nil, "", ""))
	if !bytes.Equal(out, frame) {
		t.Fatalf("a declining injector rewrote the frame\n in: %s\nout: %s", frame, out)
	}
	got, ok := metaValue(out, mcp.MetaAllowDirsKey)
	if !ok {
		t.Fatal("premise broken: the client's own _meta grant is not in the fixture")
	}
	if string(got) != `["/","/etc"]` {
		t.Errorf("client _meta grant = %s, want it passed through unchanged", got)
	}

	// And the property that DOES hold: once the proxy has a grant, its value wins
	// over the client's, whatever the client wrote.
	withGrant := injectInitMeta(frame, buildInitMeta(proxyDirs, "", ""))
	got, ok = metaValue(withGrant, mcp.MetaAllowDirsKey)
	if !ok {
		t.Fatal("the proxy's grant is absent after injection")
	}
	var dirs []string
	if err := json.Unmarshal(got, &dirs); err != nil {
		t.Fatalf("allow-dirs is not a string array: %s", got)
	}
	if len(dirs) != len(proxyDirs) || dirs[0] != proxyDirs[0] {
		t.Errorf("client kept influence over the grant: got %v, want the proxy's %v", dirs, proxyDirs)
	}
}

// TestIDKey_DistinctLargeIDsCollide records the KNOWN, unfixed defect the
// injectivity oracle found, so it is a documented fact with a payload rather
// than a note in a commit message nobody greps.
//
// It asserts the CURRENT behaviour deliberately. When internal/mcp starts
// carrying ids as json.RawMessage and idKey can safely preserve digits, this
// test fails — which is the intended signal to delete it and restore the
// injectivity property, not to adjust it.
func TestIDKey_DistinctLargeIDsCollide(t *testing.T) {
	t.Parallel()
	a := json.RawMessage(`10000000000000000001`)
	b := json.RawMessage(`10000000000000000002`)
	if idKey(a) != idKey(b) {
		t.Fatalf("idKey now distinguishes large integer ids (%q vs %q).\n"+
			"If internal/mcp now carries request ids as json.RawMessage, delete this test "+
			"and assert injectivity instead — see FuzzIDKeyRoutesResponsesToRequests.",
			idKey(a), idKey(b))
	}
	// And the reason it cannot simply be fixed here: the daemon has already lost
	// the distinction before the proxy sees the response.
	ea, _ := daemonEchoedID(a)
	eb, _ := daemonEchoedID(b)
	if !bytes.Equal(ea, eb) {
		t.Errorf("the daemon now preserves these ids (%s vs %s) — the proxy-side fix is "+
			"newly viable and this test should be revisited", ea, eb)
	}
}
