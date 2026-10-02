package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// resumeconsumer_test.go pins the daemon's READ of the resume-credential consumer
// announcement. The daemon discloses a credential only to a proxy that announced it
// strips the key, so this read is the one switch between "inert" and "a bearer secret
// in a client transcript". Both directions are required: an announcement must ARRIVE,
// and nothing that is not exactly the announcement may fire.
func TestInitialize_ReadsTheResumeCredentialConsumerAnnouncement(t *testing.T) {
	cases := []struct {
		name      string
		params    string
		wantFired bool
	}{
		{"the number 1 is the announcement", `{"_meta":{"dev.plumbkit/resume-credential-consumer":1}}`, true},
		{"no key does not fire", `{"_meta":{"dev.plumbkit/proxy-session-id":"proxyX"}}`, false},
		{"no _meta at all does not fire", `{"clientInfo":{"name":"someclient","version":"1"}}`, false},
		{"a string is not the announcement", `{"_meta":{"dev.plumbkit/resume-credential-consumer":"1"}}`, false},
		{"a boolean is not the announcement", `{"_meta":{"dev.plumbkit/resume-credential-consumer":true}}`, false},
		{"zero does not fire", `{"_meta":{"dev.plumbkit/resume-credential-consumer":0}}`, false},
		{"another number does not fire", `{"_meta":{"dev.plumbkit/resume-credential-consumer":2}}`, false},
		{"an object does not fire", `{"_meta":{"dev.plumbkit/resume-credential-consumer":{"x":1}}}`, false},
		{"malformed params do not fire", `{"_meta":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(ServerInfo{Name: "plumb", Version: "0.21.0"})
			var fired bool
			s.OnResumeCredentialConsumer = func(context.Context) { fired = true }

			s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: json.RawMessage(tc.params)})

			if fired != tc.wantFired {
				t.Fatalf("OnResumeCredentialConsumer fired = %v, want %v", fired, tc.wantFired)
			}
		})
	}
}

// The announcement is delivered BEFORE the proxy session hook, which is what settles
// identity and mints: a connection must know whether it may be disclosed anything
// before it is first issued something.
func TestInitialize_ResumeCredentialConsumerPrecedesTheProxySession(t *testing.T) {
	s := New(ServerInfo{Name: "plumb", Version: "0.21.0"})
	var order []string
	s.OnResumeCredentialConsumer = func(context.Context) { order = append(order, "consumer") }
	s.OnProxySession = func(context.Context, string) { order = append(order, "proxy-session") }

	params := json.RawMessage(`{"_meta":{` +
		`"dev.plumbkit/proxy-session-id":"proxyX",` +
		`"dev.plumbkit/resume-credential-consumer":1}}`)
	s.handleInitialize(context.Background(), mcpRequest{ID: 1, Params: params})

	if len(order) != 2 || order[0] != "consumer" || order[1] != "proxy-session" {
		t.Fatalf("hook order = %v, want [consumer proxy-session]", order)
	}
}

func TestResumeCredentialConsumerKeyName(t *testing.T) {
	if MetaResumeCredentialConsumerKey != "dev.plumbkit/resume-credential-consumer" {
		t.Fatalf("MetaResumeCredentialConsumerKey = %q", MetaResumeCredentialConsumerKey)
	}
}
