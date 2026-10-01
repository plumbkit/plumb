package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

func TestResultNotes_RoundTripAndAbsence(t *testing.T) {
	ctx := mcp.WithResultNotes(context.Background())
	if v, ok := mcp.ResultMetaNote(ctx, "k"); ok || v != "" {
		t.Fatalf("an unwritten key reads (%q, %v), want absent", v, ok)
	}
	mcp.NoteResultMeta(ctx, "k", "first")
	mcp.NoteResultMeta(ctx, "k", "second")
	if v, ok := mcp.ResultMetaNote(ctx, "k"); !ok || v != "second" {
		t.Errorf("ResultMetaNote = (%q, %v), want the later note", v, ok)
	}
}

// A ctx with no scratchpad is an in-process call: recording is a silent no-op,
// and reading says "not reported" rather than inventing a value.
func TestResultNotes_NoScratchpadIsANoOp(t *testing.T) {
	ctx := context.Background()
	mcp.NoteResultMeta(ctx, "k", "v")
	if v, ok := mcp.ResultMetaNote(ctx, "k"); ok || v != "" {
		t.Errorf("a ctx with no scratchpad reads (%q, %v), want absent", v, ok)
	}
}

// noteTool records what it is told in the call's result notes, the way
// session_start's re-pin callback records which pin it moved.
type noteTool struct{ value string }

func (noteTool) Name() string        { return "note_tool" }
func (noteTool) Description() string { return "records a result note" }
func (noteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (n noteTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if n.value != "" {
		mcp.NoteResultMeta(ctx, "dev.test/note", n.value)
	}
	return "ok", nil
}

// A note made by a tool must reach ToolResultMeta through the real tools/call
// handler: the handler mints the scratchpad, the tool writes it, the hook reads
// it. Each call gets its own.
func TestServer_ToolResultMeta_SeesWhatTheToolNoted(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(noteTool{value: "from-the-tool"})
	s.Register(quietTool{})
	s.ToolResultMeta = func(ctx context.Context, name string, _ json.RawMessage) map[string]any {
		v, _ := mcp.ResultMetaNote(ctx, "dev.test/note")
		return map[string]any{"tool": name, "noted": v}
	}

	resps := serveOn(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"note_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"quiet_tool","arguments":{}}}`,
	)
	// Responses are not in request order, so they are matched by id.
	metaOf := func(id float64) map[string]any {
		for _, r := range resps {
			if r["id"] != id {
				continue
			}
			result, _ := r["result"].(map[string]any)
			meta, _ := result["_meta"].(map[string]any)
			return meta
		}
		t.Fatalf("no response with id %v in %v", id, resps)
		return nil
	}
	if got := metaOf(1)["noted"]; got != "from-the-tool" {
		t.Fatalf("ToolResultMeta read %v, want what the tool noted", got)
	}
	// Per call, not per server: a tool that notes nothing leaves nothing behind
	// for the call beside it.
	if got := metaOf(2)["noted"]; got != "" {
		t.Errorf("a later call that noted nothing read %v: the scratchpad outlived its call", got)
	}
}

// quietTool notes nothing.
type quietTool struct{}

func (quietTool) Name() string        { return "quiet_tool" }
func (quietTool) Description() string { return "notes nothing" }
func (quietTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (quietTool) Execute(context.Context, json.RawMessage) (string, error) { return "ok", nil }
