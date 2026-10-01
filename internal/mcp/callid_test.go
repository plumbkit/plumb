package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

func TestNewULIDMatchesTheSpecVector(t *testing.T) {
	// ULID spec README: timestamp 1469918176385 encodes as 01ARYZ6S41.
	id := newULID(time.UnixMilli(1469918176385), bytes.NewReader(make([]byte, 10)))
	if id != "01ARYZ6S41"+"0000000000000000" {
		t.Fatalf("newULID = %q", id)
	}
}

func TestCallIDsSortByTimeAndDiffer(t *testing.T) {
	a := newULID(time.UnixMilli(1000), bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))
	b := newULID(time.UnixMilli(1001), bytes.NewReader(make([]byte, 10)))
	if a >= b {
		t.Fatalf("ids not time-ordered: %q !< %q", a, b)
	}
	if x, y := NewCallID(), NewCallID(); x == y || len(x) != 26 {
		t.Fatalf("NewCallID: %q, %q", x, y)
	}
}

func TestCallIDContextRoundTrip(t *testing.T) {
	if got := CallIDFromCtx(context.Background()); got != "" {
		t.Fatalf("empty ctx carried %q", got)
	}
	if got := CallIDFromCtx(WithCallID(context.Background(), "X")); got != "X" {
		t.Fatalf("got %q", got)
	}
}

type captureCallIDTool struct {
	lastID string
}

func (c *captureCallIDTool) Name() string        { return "capture_call_id" }
func (c *captureCallIDTool) Description() string { return "captures call id" }
func (c *captureCallIDTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (c *captureCallIDTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	c.lastID = CallIDFromCtx(ctx)
	return "ok", nil
}

func TestCallIDConsistentAcrossToolAndHooks(t *testing.T) {
	s := New(ServerInfo{Name: "test", Version: "0"})
	tool := &captureCallIDTool{}
	s.Register(tool)

	var beforeID, afterID string
	s.OnBeforeTool = func(ctx context.Context, name string, args json.RawMessage, logicalAgent string) {
		beforeID = CallIDFromCtx(ctx)
	}
	s.OnAfterTool = func(ctx context.Context, name string, args json.RawMessage, output, errMsg string, dur time.Duration, isError bool, failure *toolerror.Error) {
		afterID = CallIDFromCtx(ctx)
	}

	req1 := mcpRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"capture_call_id","arguments":{}}`),
	}
	resp1 := s.handleToolsCall(context.Background(), req1)
	if resp1.Error != nil {
		t.Fatalf("call 1 failed: %v", resp1.Error)
	}
	if beforeID == "" {
		t.Fatal("expected non-empty call_id in OnBeforeTool")
	}
	if tool.lastID != beforeID {
		t.Fatalf("tool got %q, want %q", tool.lastID, beforeID)
	}
	if afterID != beforeID {
		t.Fatalf("OnAfterTool got %q, want %q", afterID, beforeID)
	}

	firstID := beforeID

	req2 := mcpRequest{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"capture_call_id","arguments":{}}`),
	}
	resp2 := s.handleToolsCall(context.Background(), req2)
	if resp2.Error != nil {
		t.Fatalf("call 2 failed: %v", resp2.Error)
	}
	if beforeID == "" {
		t.Fatal("expected non-empty call_id in second call")
	}
	if beforeID == firstID {
		t.Fatalf("two calls got same call_id %q", firstID)
	}
	if tool.lastID != beforeID || afterID != beforeID {
		t.Fatalf("mismatched IDs in second call: before=%q tool=%q after=%q", beforeID, tool.lastID, afterID)
	}
}
