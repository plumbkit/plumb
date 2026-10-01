package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// advertisedSchemas serves tools/list on s and returns each tool's inputSchema.
func advertisedSchemas(t *testing.T, s *mcp.Server) map[string]map[string]any {
	t.Helper()
	resps := serveOn(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	list, _ := resultByID(t, resps, 1)["tools"].([]any)
	out := map[string]map[string]any{}
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		schema, _ := tool["inputSchema"].(map[string]any)
		out[name] = schema
	}
	if len(out) == 0 {
		t.Fatal("tools/list advertised no tools")
	}
	return out
}

func declaredProps(schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	return props
}

// TestToolsList_DeclaresIdentityPropertyOnlyWhenAsked: the property is added to
// every tool, next to the tool's own properties, only for a connection whose
// client needs it.
func TestToolsList_DeclaresIdentityPropertyOnlyWhenAsked(t *testing.T) {
	s, _, _ := identityServer(t)
	for name, schema := range advertisedSchemas(t, s) {
		if _, ok := declaredProps(schema)[mcp.ArgLogicalAgentDeclaredKey]; ok {
			t.Errorf("%s declares %s with DeclareIdentityArg unset", name, mcp.ArgLogicalAgentDeclaredKey)
		}
	}
	s.DeclareIdentityArg = func() bool { return false }
	for name, schema := range advertisedSchemas(t, s) {
		if _, ok := declaredProps(schema)[mcp.ArgLogicalAgentDeclaredKey]; ok {
			t.Errorf("%s declares %s with DeclareIdentityArg false", name, mcp.ArgLogicalAgentDeclaredKey)
		}
	}
	plain := advertisedSchemas(t, s)["rename_thing"]
	s.DeclareIdentityArg = func() bool { return true }
	schemas := advertisedSchemas(t, s)
	for name, schema := range schemas {
		if _, ok := declaredProps(schema)[mcp.ArgLogicalAgentDeclaredKey]; !ok {
			t.Errorf("%s does not declare %s with DeclareIdentityArg true: %v", name, mcp.ArgLogicalAgentDeclaredKey, schema)
		}
	}
	if _, ok := declaredProps(schemas["rename_thing"])["name"]; !ok {
		t.Errorf("the tool's own property was lost: %v", schemas["rename_thing"])
	}
	if got, want := schemas["rename_thing"]["additionalProperties"], plain["additionalProperties"]; got != want {
		t.Errorf("additionalProperties changed from %v to %v", want, got)
	}
}

// TestToolsCall_DeclaredKeyIsLifted: the declarable key carries an identity and
// is stripped before the closed-schema guard, like the reverse-DNS key.
func TestToolsCall_DeclaredKeyIsLifted(t *testing.T) {
	s, agent, args := identityServer(t)
	resps := serveOn(t, s, callWith("rename_thing", `{"name":"x","`+mcp.ArgLogicalAgentDeclaredKey+`":"a1"}`))
	if result := resultByID(t, resps, 1); result["isError"] == true || toolText(result) != "renamed to x" {
		t.Fatalf("a call stamped with the declarable key must run unchanged, got %v", result)
	}
	if *agent != "a1" {
		t.Fatalf("LogicalAgentFromCtx = %q, want a1", *agent)
	}
	if strings.Contains(string(*args), mcp.ArgLogicalAgentDeclaredKey) {
		t.Fatalf("hooks must see stripped arguments, got %s", *args)
	}
}

// TestToolsCall_ReverseDNSKeyOutranksDeclaredKey pins the precedence when a
// caller sends both, and that both are stripped.
func TestToolsCall_ReverseDNSKeyOutranksDeclaredKey(t *testing.T) {
	s, agent, args := identityServer(t)
	resps := serveOn(t, s, callWith("rename_thing",
		`{"name":"x","`+mcp.ArgLogicalAgentKey+`":"rdns","`+mcp.ArgLogicalAgentDeclaredKey+`":"declared"}`))
	if result := resultByID(t, resps, 1); result["isError"] == true {
		t.Fatalf("call refused: %v", result)
	}
	if *agent != "rdns" {
		t.Fatalf("LogicalAgentFromCtx = %q, want rdns", *agent)
	}
	if strings.Contains(string(*args), "declared") || strings.Contains(string(*args), "rdns") {
		t.Fatalf("both keys must be stripped, got %s", *args)
	}
}

// stripUndeclared forwards only the argument keys the advertised schema
// declares, which is what Claude desktop's connector does to a tool call.
func stripUndeclared(t *testing.T, schema map[string]any, args map[string]any) string {
	t.Helper()
	props := declaredProps(schema)
	kept := map[string]any{}
	for k, v := range args {
		if _, ok := props[k]; ok {
			kept[k] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStrippingHost_IdentitySurvivesOnlyWhenDeclared reproduces the
// 2026-09-30 incident at the protocol level. The control shows the host
// simulation really strips: the reverse-DNS stamp is lost and the call arrives
// anonymous, which is what sent a worktree edit to the main checkout. With the
// declarable key advertised and stamped, the identity arrives.
func TestStrippingHost_IdentitySurvivesOnlyWhenDeclared(t *testing.T) {
	s, agent, _ := identityServer(t)
	s.DeclareIdentityArg = func() bool { return true }
	schema := advertisedSchemas(t, s)["rename_thing"]

	lost := stripUndeclared(t, schema, map[string]any{"name": "x", mcp.ArgLogicalAgentKey: "conv-1"})
	serveOn(t, s, callWith("rename_thing", lost))
	if *agent != "" {
		t.Fatalf("control: the host simulation did not strip the undeclared stamp (agent %q)", *agent)
	}

	kept := stripUndeclared(t, schema, map[string]any{"name": "x", mcp.ArgLogicalAgentDeclaredKey: "conv-1"})
	serveOn(t, s, callWith("rename_thing", kept))
	if *agent != "conv-1" {
		t.Fatalf("declared stamp did not survive the stripping host: agent %q (args %s)", *agent, kept)
	}
}

// servedEntryBytes serves tools/list on s and returns, per tool, the byte size
// of its entry as served — name, description and inputSchema, re-encoded with
// the field set ToolSchemaBytes measures (the `_meta` block excluded).
func servedEntryBytes(t *testing.T, s *mcp.Server) map[string]int {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	type entry struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	var resp struct {
		Result struct {
			Tools []entry `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	sizes := map[string]int{}
	for _, e := range resp.Result.Tools {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sizes[e.Name] = len(b)
	}
	if len(sizes) == 0 {
		t.Fatal("tools/list advertised no tools")
	}
	return sizes
}

// TestToolSchemaBytes_MatchesTheAdvertisedSchemas (#515): the size reported
// for the surcharge is the size of what this connection is actually served,
// so a connection that gets the declared identity property is charged for it
// on every tool and one that does not is not.
func TestToolSchemaBytes_MatchesTheAdvertisedSchemas(t *testing.T) {
	s, _, _ := identityServer(t)
	plain := s.ToolSchemaBytes()
	for _, declare := range []bool{false, true} {
		s.DeclareIdentityArg = func() bool { return declare }
		got, served := s.ToolSchemaBytes(), servedEntryBytes(t, s)
		if len(got) != len(served) {
			t.Fatalf("declare=%v: ToolSchemaBytes covers %d tools, tools/list serves %d", declare, len(got), len(served))
		}
		for name, want := range served {
			if got[name] != want {
				t.Errorf("declare=%v: %s reported as %d bytes, served as %d", declare, name, got[name], want)
			}
			grew := got[name] > plain[name]
			if grew != declare {
				t.Errorf("declare=%v: %s is %d bytes against %d undecorated", declare, name, got[name], plain[name])
			}
		}
	}
}
