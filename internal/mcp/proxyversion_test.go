package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// proxyversion_test.go pins the daemon's READ of the proxy version, which is the
// link an independent review found untested: deleting the one line that fires
// OnProxyVersion left the entire suite green.
//
// That gap is worse than an ordinary one, and the reason is worth keeping. With
// the read gone, daemon_info prints "unknown — no serve proxy declared one",
// which is exactly what a legitimately-old proxy produces. Broken and
// correct-degraded are the same string, so a green suite could not distinguish
// the feature working from the feature silently not working — and the symptom of
// failure was the reassurance the row exists to give.
//
// Both directions are therefore required, and only the second was reachable
// before: a declared version must ARRIVE (known-true true), and an undeclared one
// must not fire at all (known-false false).

func TestInitialize_ReadsTheProxyVersion(t *testing.T) {
	cases := []struct {
		name      string
		params    string
		wantFired bool
		wantValue string
	}{
		{
			// The load-bearing direction. Delete the dispatch and this fails.
			name:      "declared version arrives",
			params:    `{"_meta":{"dev.plumbkit/proxy-version":"0.19.2"}}`,
			wantFired: true,
			wantValue: "0.19.2",
		},
		{
			// A proxy older than the key, or a direct client. Must not fire, so
			// the tool can say "unknown" rather than report a false empty match.
			name:      "no key does not fire",
			params:    `{"_meta":{"dev.plumbkit/proxy-session-id":"proxyX"}}`,
			wantFired: false,
		},
		{
			name:      "no _meta at all does not fire",
			params:    `{"clientInfo":{"name":"someclient","version":"1"}}`,
			wantFired: false,
		},
		{
			// An empty string is "nobody said", not "the versions match".
			name:      "empty value does not fire",
			params:    `{"_meta":{"dev.plumbkit/proxy-version":""}}`,
			wantFired: false,
		},
		{
			// Fail-safe, like every other _meta reader: a wrong-typed value is
			// not a version, and must not crash or half-report.
			name:      "wrong type does not fire",
			params:    `{"_meta":{"dev.plumbkit/proxy-version":["0.19.2"]}}`,
			wantFired: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(ServerInfo{Name: "plumb", Version: "0.19.3"})
			var got string
			var fired bool
			s.OnProxyVersion = func(_ context.Context, v string) {
				fired, got = true, v
			}

			s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: json.RawMessage(tc.params)})

			if fired != tc.wantFired {
				t.Fatalf("OnProxyVersion fired = %v, want %v (value %q)", fired, tc.wantFired, got)
			}
			if fired && got != tc.wantValue {
				t.Errorf("OnProxyVersion got %q, want %q", got, tc.wantValue)
			}
		})
	}
}

// TestInitialize_ProxyVersionIsIndependentOfTheSessionKey guards the mistake
// that would make the read look wired while reporting the wrong thing: these two
// keys are both strings, both proxy-supplied, and adjacent in the same object.
func TestInitialize_ProxyVersionIsIndependentOfTheSessionKey(t *testing.T) {
	s := New(ServerInfo{Name: "plumb", Version: "0.19.3"})
	var version, session string
	s.OnProxyVersion = func(_ context.Context, v string) { version = v }
	s.OnProxySession = func(_ context.Context, id string) { session = id }

	params := json.RawMessage(`{"_meta":{` +
		`"dev.plumbkit/proxy-version":"0.19.2",` +
		`"dev.plumbkit/proxy-session-id":"proxyX"}}`)
	s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: params})

	if version != "0.19.2" {
		t.Errorf("proxy version = %q, want 0.19.2", version)
	}
	if session != "proxyX" {
		t.Errorf("proxy session = %q, want proxyX", session)
	}
}
