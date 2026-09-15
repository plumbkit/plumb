package cli

import (
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// serve_proxyversion_test.go covers the proxy half of "which version am I
// running?" — the question daemon_info used to answer about the wrong binary.
//
// The proxy is the only party that knows its own version: `plumb restart`
// replaces the daemon while every attached proxy keeps the binary it launched
// with. So the value has to travel on the wire, and it travels in the captured
// initialize frame specifically, because that frame is replayed on every
// reconnect — which is the reconnect the mismatch is created by.

// TestBuildInitMeta_CarriesProxyVersion pins that the version is sent at all.
func TestBuildInitMeta_CarriesProxyVersion(t *testing.T) {
	meta := buildInitMeta(nil, "", "", "9.9.9")
	raw, ok := meta[mcp.MetaProxyVersionKey]
	if !ok {
		t.Fatalf("no %s in _meta: %v", mcp.MetaProxyVersionKey, meta)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("proxy version is not a JSON string: %v", err)
	}
	if got != "9.9.9" {
		t.Errorf("proxy version = %q, want 9.9.9", got)
	}
}

// TestBuildInitMeta_MixedValueTypesStayDecodable is the regression for what this
// change broke on its way in, and it is a property of _meta rather than of this
// key.
//
// _meta is one shared object whose values have DIFFERENT types: allow-dirs is an
// array, the proxy session id and now the proxy version are strings. A reader
// that decodes it as map[string][]string unmarshals nothing at all once a string
// value is present — not "the key is missing", the WHOLE object fails — and then
// reports the array key as absent. That is exactly how adding this key first
// showed up: as "the initial daemon did not receive the allow-dir".
//
// Every production reader already uses map[string]json.RawMessage. This pins the
// property so the next key added here does not have to rediscover it.
func TestBuildInitMeta_MixedValueTypesStayDecodable(t *testing.T) {
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out := injectInitMeta(frame, buildInitMeta([]string{"/granted"}, "sess-1", "/ws", "9.9.9"))

	var p struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("augmented frame does not decode: %v", err)
	}
	// The array-valued key must still be readable alongside the string-valued ones.
	var dirs []string
	if err := json.Unmarshal(p.Params.Meta[mcp.MetaAllowDirsKey], &dirs); err != nil {
		t.Fatalf("allow-dirs unreadable beside a string-valued key: %v", err)
	}
	if len(dirs) != 1 || dirs[0] != "/granted" {
		t.Errorf("allow-dirs = %v, want [/granted]", dirs)
	}
	for _, key := range []string{mcp.MetaProxySessionKey, mcp.MetaProxyVersionKey} {
		var s string
		if err := json.Unmarshal(p.Params.Meta[key], &s); err != nil || s == "" {
			t.Errorf("%s unreadable or empty: %v", key, err)
		}
	}
}

// TestBuildInitMeta_NoVersionSendsNoKey keeps the zero-cost path honest: a caller
// with nothing to say must produce a byte-identical frame, so a session that
// declares no version behaves exactly as it did before this key existed.
func TestBuildInitMeta_NoVersionSendsNoKey(t *testing.T) {
	if meta := buildInitMeta(nil, "", "", ""); meta != nil {
		t.Fatalf("buildInitMeta with nothing to send = %v, want nil", meta)
	}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if got := injectInitMeta(frame, buildInitMeta(nil, "", "", "")); string(got) != string(frame) {
		t.Errorf("frame changed with nothing to inject:\n got %s\nwant %s", got, frame)
	}
}
