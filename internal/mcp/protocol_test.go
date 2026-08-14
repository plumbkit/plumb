package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
)

func TestNegotiateProtocolVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		offered string
		want    string
	}{
		{name: "offered in set", offered: "2024-11-05", want: "2024-11-05"},
		{name: "offered newer than supported", offered: "2025-11-25", want: "2024-11-05"},
		{name: "empty offer", offered: "", want: "2024-11-05"},
		{name: "garbage offer", offered: "not-a-version", want: "2024-11-05"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := negotiateProtocolVersion(c.offered); got != c.want {
				t.Errorf("negotiateProtocolVersion(%q) = %q, want %q", c.offered, got, c.want)
			}
		})
	}
}

// TestHandleInitialize_NegotiatesProtocol pins the response half of the
// handshake: a client offering a revision plumb has not implemented gets the
// newest SUPPORTED revision back — never an echo of its own offer, which would
// be a false claim of support. The hook half lives in
// TestDispatchInitialize_ProtocolHookFiresOnce because the hook fires under the
// per-connection once-guard in dispatchMessage, not in handleInitialize.
func TestHandleInitialize_NegotiatesProtocol(t *testing.T) {
	t.Parallel()
	srv := New(ServerInfo{Name: "test", Version: "0"})

	resp, isRequest := srv.handle(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{`+
			`"protocolVersion":"2025-11-25",`+
			`"capabilities":{"roots":{"listChanged":true},"elicitation":{}}}}`))
	if !isRequest {
		t.Fatal("initialize did not produce a response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("answered protocolVersion = %v, want 2024-11-05", result["protocolVersion"])
	}
}

// TestDispatchInitialize_ProtocolHookFiresOnce pins the once-per-connection
// contract of OnProtocolNegotiated: a client that re-sends initialize on one
// connection must not double-record (or double-log) the negotiation, and the
// hook observes the offered revision plus the raw advertised capabilities.
func TestDispatchInitialize_ProtocolHookFiresOnce(t *testing.T) {
	t.Parallel()
	srv := New(ServerInfo{Name: "test", Version: "0"})
	var calls int
	var gotOffered, gotAnswered string
	var gotCaps json.RawMessage
	srv.OnProtocolNegotiated = func(_ context.Context, offered, answered string, caps json.RawMessage) {
		calls++
		gotOffered, gotAnswered, gotCaps = offered, answered, caps
	}
	ss := newServeState(srv, io.Discard)
	var initOnce sync.Once
	msg := []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
			`"protocolVersion":"2025-11-25",` +
			`"capabilities":{"roots":{"listChanged":true},"elicitation":{}}}}`)
	ss.dispatchMessage(context.Background(), msg, &initOnce)
	ss.dispatchMessage(context.Background(), msg, &initOnce)
	if calls != 1 {
		t.Fatalf("OnProtocolNegotiated fired %d times over two initialize requests, want 1", calls)
	}
	if gotOffered != "2025-11-25" {
		t.Errorf("hook offered = %q, want 2025-11-25", gotOffered)
	}
	if gotAnswered != "2024-11-05" {
		t.Errorf("hook answered = %q, want 2024-11-05", gotAnswered)
	}
	wantCaps := `{"roots":{"listChanged":true},"elicitation":{}}`
	if string(gotCaps) != wantCaps {
		t.Errorf("hook capabilities = %s, want %s", gotCaps, wantCaps)
	}
}

// TestClientProtocolParams covers the fail-safe extraction contract: any shape
// mismatch yields "" / nil, and negotiation then answers with the newest
// supported revision.
func TestClientProtocolParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		params   string
		wantVer  string
		wantCaps string
	}{
		{
			name:     "version and capabilities",
			params:   `{"protocolVersion":"2024-11-05","capabilities":{"sampling":{}}}`,
			wantVer:  "2024-11-05",
			wantCaps: `{"sampling":{}}`,
		},
		{name: "no protocolVersion", params: `{"capabilities":{}}`, wantVer: "", wantCaps: `{}`},
		{name: "no capabilities", params: `{"protocolVersion":"2024-11-05"}`, wantVer: "2024-11-05", wantCaps: ""},
		{name: "null capabilities", params: `{"capabilities":null}`, wantVer: "", wantCaps: ""},
		{
			name:     "type-mismatched version keeps capabilities",
			params:   `{"protocolVersion":123,"capabilities":{"sampling":{}}}`,
			wantVer:  "",
			wantCaps: `{"sampling":{}}`,
		},
		{name: "empty params", params: `{}`, wantVer: "", wantCaps: ""},
		{name: "malformed", params: `not json`, wantVer: "", wantCaps: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ver, caps := clientProtocolParams(json.RawMessage(c.params))
			if ver != c.wantVer {
				t.Errorf("version = %q, want %q", ver, c.wantVer)
			}
			if string(caps) != c.wantCaps {
				t.Errorf("capabilities = %q, want %q", caps, c.wantCaps)
			}
		})
	}
	// Nil params (an initialize with no params member at all) must not panic.
	if ver, caps := clientProtocolParams(nil); ver != "" || caps != nil {
		t.Errorf("nil params: got (%q, %s), want (\"\", nil)", ver, caps)
	}
}
