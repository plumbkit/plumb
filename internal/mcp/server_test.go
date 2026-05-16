package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/mcp"
)

// echoTool echoes its "text" argument — minimal Tool for server tests.
type echoTool struct{}

func (e *echoTool) Name() string             { return "echo" }
func (e *echoTool) Description() string      { return "echoes the text argument" }
func (e *echoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)
}
func (e *echoTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Text, nil
}

func newServer() *mcp.Server {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(&echoTool{})
	return s
}

// serve runs one or more newline-separated requests through the server and
// returns the parsed responses in order.
func serve(t *testing.T, requests ...string) []map[string]any {
	t.Helper()
	input := strings.Join(requests, "\n") + "\n"
	var out bytes.Buffer
	if err := newServer().Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	dec := json.NewDecoder(&out)
	var results []map[string]any
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		results = append(results, m)
	}
	return results
}

func TestServer_Initialize(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	r := resps[0]
	if r["error"] != nil {
		t.Fatalf("unexpected error: %v", r["error"])
	}
	result, ok := r["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", r["result"])
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("unexpected protocolVersion: %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "test" {
		t.Fatalf("unexpected serverInfo.name: %v", info["name"])
	}
	instr, _ := result["instructions"].(string)
	if !strings.Contains(instr, "session_start") {
		t.Fatalf("instructions field missing or doesn't mention session_start: %q", instr)
	}
}

func TestServer_Initialize_CustomInstructions(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0", Instructions: "custom"})
	s.Register(&echoTool{})
	var out bytes.Buffer
	_ = s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	dec := json.NewDecoder(&out)
	var resp map[string]any
	_ = dec.Decode(&resp)
	result, _ := resp["result"].(map[string]any)
	if result["instructions"] != "custom" {
		t.Fatalf("want custom instructions, got %v", result["instructions"])
	}
}

func TestServer_Initialize_SuppressedInstructions(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0", Instructions: "-"})
	s.Register(&echoTool{})
	var out bytes.Buffer
	_ = s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &out)
	dec := json.NewDecoder(&out)
	var resp map[string]any
	_ = dec.Decode(&resp)
	result, _ := resp["result"].(map[string]any)
	if _, present := result["instructions"]; present {
		t.Fatalf("instructions should be omitted when set to \"-\", got %v", result["instructions"])
	}
}

func TestServer_Ping(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if resps[0]["error"] != nil {
		t.Fatalf("unexpected error: %v", resps[0]["error"])
	}
}

func TestServer_ToolsList(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result, _ := resps[0]["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "echo" {
		t.Fatalf("unexpected tool name: %v", tool["name"])
	}
	if tool["inputSchema"] == nil {
		t.Fatal("expected inputSchema to be present")
	}
}

func TestServer_ToolsCall(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result, _ := resps[0]["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("unexpected isError: true")
	}
	contents, _ := result["content"].([]any)
	if len(contents) != 1 {
		t.Fatalf("want 1 content item, got %d", len(contents))
	}
	item, _ := contents[0].(map[string]any)
	if item["text"] != "hello" {
		t.Fatalf("got text %q, want %q", item["text"], "hello")
	}
}

func TestServer_ToolsCall_UnknownTool(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if resps[0]["error"] == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestServer_MethodNotFound(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":6,"method":"no/such/method"}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if resps[0]["error"] == nil {
		t.Fatal("expected error response for unknown method")
	}
}

func TestServer_Notification_NoResponse(t *testing.T) {
	// Notification has no ID — server must not write a response.
	resps := serve(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(resps) != 0 {
		t.Fatalf("want 0 responses for notification, got %d", len(resps))
	}
}

func TestServer_MultipleRequests(t *testing.T) {
	resps := serve(t,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resps))
	}
}
