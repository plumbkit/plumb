package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// outlineTool stands in for file_outline — the canonical tool behind the
// retired list_symbols alias, whose adapter re-marshals the arguments — with a
// closed schema, so an argument that survived the alias adapter would be
// rejected by the guard.
type outlineTool struct{}

func (outlineTool) Name() string        { return "file_outline" }
func (outlineTool) Description() string { return "outlines a file" }
func (outlineTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string"}},"required":["uri"],"additionalProperties":false}`)
}

func (outlineTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(args, &a)
	return "outlined " + a.URI, nil
}

func identityServer(t *testing.T) (*mcp.Server, *string, *json.RawMessage) {
	t.Helper()
	s := newServer()
	s.Register(strictTool{})
	s.Register(outlineTool{})
	var seenAgent string
	var seenArgs json.RawMessage
	s.OnBeforeTool = func(ctx context.Context, _ string, args json.RawMessage, _ string) {
		seenAgent = mcp.LogicalAgentFromCtx(ctx)
		seenArgs = append(json.RawMessage(nil), args...)
	}
	return s, &seenAgent, &seenArgs
}

func callWith(name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
}

// TestToolsCall_ReservedArgumentIsStrippedBeforeValidation: a closed-schema
// tool served with the reserved key runs as if the key were never there, and
// the identity reaches the ctx the hooks and the tool see.
func TestToolsCall_ReservedArgumentIsStrippedBeforeValidation(t *testing.T) {
	s, agent, args := identityServer(t)
	resps := serveOn(t, s, callWith("rename_thing", `{"name":"x","`+mcp.ArgLogicalAgentKey+`":"a1"}`))
	result := resultByID(t, resps, 1)
	if result["isError"] == true || toolText(result) != "renamed to x" {
		t.Fatalf("stamped call must run unchanged, got %v", result)
	}
	if *agent != "a1" {
		t.Fatalf("LogicalAgentFromCtx = %q, want a1", *agent)
	}
	if strings.Contains(string(*args), mcp.ArgLogicalAgentKey) {
		t.Fatalf("hooks must see stripped arguments, got %s", *args)
	}
}

// TestToolsCall_OtherUnknownParamsStillRejected: the strip takes exactly one
// key. Any other undeclared parameter is still the guard's business.
func TestToolsCall_OtherUnknownParamsStillRejected(t *testing.T) {
	s, _, _ := identityServer(t)
	resps := serveOn(t, s, callWith("rename_thing", `{"name":"x","bogus":1,"`+mcp.ArgLogicalAgentKey+`":"a1"}`))
	result := resultByID(t, resps, 1)
	if result["isError"] != true || !strings.Contains(toolText(result), "bogus") {
		t.Fatalf("an unrelated unknown parameter must still be rejected by name, got %v", result)
	}
}

// TestToolsCall_MetaOutranksReservedArgument pins the precedence between the
// two per-call channels.
func TestToolsCall_MetaOutranksReservedArgument(t *testing.T) {
	s, agent, _ := identityServer(t)
	both := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rename_thing","arguments":{"name":"x","%s":"from-arg"},"_meta":{"%s":"from-meta"}}}`,
		mcp.ArgLogicalAgentKey, mcp.MetaLogicalAgentKey)
	serveOn(t, s, both)
	if *agent != "from-meta" {
		t.Fatalf("_meta must outrank the argument, got %q", *agent)
	}
	serveOn(t, s, callWith("rename_thing", `{"name":"x","`+mcp.ArgLogicalAgentKey+`":"from-arg"}`))
	if *agent != "from-arg" {
		t.Fatalf("the argument must be used when _meta carries none, got %q", *agent)
	}
}

// TestToolsCall_ReservedArgumentReachesTheRefusalHook: the fail-closed ceiling
// keys on the resolved identity, so an argument-carried stamp is enough to
// attribute a call and let it through.
func TestToolsCall_ReservedArgumentReachesTheRefusalHook(t *testing.T) {
	s, _, _ := identityServer(t)
	s.OnToolRefusal = func(_ context.Context, name, logicalAgent string) error {
		if logicalAgent == "" {
			return errors.New("refused: anonymous " + name)
		}
		return nil
	}
	resps := serveOn(t, s, callWith("rename_thing", `{"name":"x","`+mcp.ArgLogicalAgentKey+`":"a1"}`))
	if result := resultByID(t, resps, 1); result["isError"] == true {
		t.Fatalf("a stamped call must be attributed, not refused: %v", result)
	}
	resps = serveOn(t, s, callWith("rename_thing", `{"name":"x"}`))
	if result := resultByID(t, resps, 1); result["isError"] != true {
		t.Fatalf("an unstamped call must still be refused: %v", result)
	}
}

// TestToolsCall_ReservedArgumentOnAliasedTool: a retired tool name whose alias
// adapter re-marshals the arguments must still be served when stamped, and
// the identity must survive the redirect. (This pins the outcome, not the
// strip's placement: dropArgs keeps unknown keys, so a strip after the adapter
// would also pass here. The placement before the adapter is for byte fidelity
// of sibling values, pinned by TestSplitLogicalAgentArg.)
func TestToolsCall_ReservedArgumentOnAliasedTool(t *testing.T) {
	s, agent, _ := identityServer(t)
	resps := serveOn(t, s, callWith("list_symbols", `{"uri":"f.go","include_signatures":true,"`+mcp.ArgLogicalAgentKey+`":"a1"}`))
	result := resultByID(t, resps, 1)
	if result["isError"] == true || !strings.Contains(toolText(result), "outlined f.go") {
		t.Fatalf("stamped aliased call must be served, got %v", result)
	}
	if *agent != "a1" {
		t.Fatalf("identity must survive the alias, got %q", *agent)
	}
}
