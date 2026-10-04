package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// A resume credential PRESENTED on a tools/call rides ctx from the request `_meta`,
// and only from there: the arguments are written by the model, and a credential the
// model could type is a claim, not the proxy's proof.
func TestResumeCredentialRidesCtxFromRequestMetaOnly(t *testing.T) {
	s := newServer()
	var got string
	s.OnBeforeTool = func(ctx context.Context, _ string, _ json.RawMessage, _ string) {
		got = mcp.ResumeCredentialFromCtx(ctx)
	}
	const secret = "rsk1-AAAAAAAAAAAAAAAAAAAAAA"

	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{
			"presented in _meta",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},"_meta":{%q:%q}}}`, mcp.MetaResumeCredentialKey, secret),
			secret,
		},
		{
			"typed into the arguments is ignored",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi",%q:%q}}}`, mcp.MetaResumeCredentialKey, secret),
			"",
		},
		{
			"a non-string value is ignored",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"},"_meta":{%q:{"x":1}}}}`, mcp.MetaResumeCredentialKey),
			"",
		},
		{
			"no key leaves the ctx empty",
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got = "stale"
			serveOn(t, s, c.frame)
			if got != c.want {
				t.Fatalf("ResumeCredentialFromCtx = %q, want %q", got, c.want)
			}
		})
	}
}

// The key is the one the design names; a proxy built against PR B writes exactly it.
func TestResumeCredentialKeyName(t *testing.T) {
	if mcp.MetaResumeCredentialKey != "dev.plumbkit/resume-credential" {
		t.Fatalf("MetaResumeCredentialKey = %q", mcp.MetaResumeCredentialKey)
	}
}
