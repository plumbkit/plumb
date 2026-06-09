package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// echoTool echoes its "text" argument — minimal Tool for server tests.
type echoTool struct{}

func (e *echoTool) Name() string        { return "echo" }
func (e *echoTool) Description() string { return "echoes the text argument" }
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

// strictTool mirrors a real tool's contract — one required, closed-set
// parameter — to exercise the dispatch-path argument guard end-to-end.
type strictTool struct{}

func (strictTool) Name() string        { return "rename_thing" }
func (strictTool) Description() string { return "renames a thing" }
func (strictTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
}

func (strictTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &a)
	return "renamed to " + a.Name, nil
}

// resultByID returns the "result" object of the response with the given id.
// Serve dispatches requests concurrently, so responses are not index-ordered.
func resultByID(t *testing.T, resps []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, r := range resps {
		if rid, ok := r["id"].(float64); ok && rid == id {
			res, _ := r["result"].(map[string]any)
			return res
		}
	}
	t.Fatalf("no response with id %v", id)
	return nil
}

// toolText returns the first text content item of a tools/call result.
func toolText(result map[string]any) string {
	contents, _ := result["content"].([]any)
	if len(contents) == 0 {
		return ""
	}
	item, _ := contents[0].(map[string]any)
	s, _ := item["text"].(string)
	return s
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
	return serveOn(t, newServer(), requests...)
}

// serveOn runs requests through a specific server, for tests that register
// tools other than echo.
func serveOn(t *testing.T, s *mcp.Server, requests ...string) []map[string]any {
	t.Helper()
	input := strings.Join(requests, "\n") + "\n"
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
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

func TestServer_EnrichToolOutput(t *testing.T) {
	s := newServer()
	s.EnrichToolOutput = func(_ context.Context, name string, _ json.RawMessage, text string) string {
		if name == "echo" {
			return text + "\n[Hint: relevant memory 'auth-gotchas' — call read_memory to view.]"
		}
		return text
	}
	resps := serveOn(t, s, `{"jsonrpc":"2.0","id":71,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`)
	result, _ := resps[0]["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("unexpected isError")
	}
	out := toolText(result)
	if !strings.Contains(out, "hello") || !strings.Contains(out, "[Hint: relevant memory 'auth-gotchas'") {
		t.Fatalf("enrichment not applied to successful output: %q", out)
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

func TestServer_ToolsCall_AliasesAndGuardsParameters(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(strictTool{})
	resps := serveOn(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rename_thing","arguments":{"new_name":"x"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"rename_thing","arguments":{"name":"x"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rename_thing","arguments":{"zzz":"x"}}}`,
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resps))
	}

	// id 1: the known alias new_name is applied, the call succeeds, and a note
	// nudges toward the canonical name — no failed call.
	r0 := resultByID(t, resps, 1)
	if r0["isError"].(bool) {
		t.Fatalf("alias should succeed, got error: %s", toolText(r0))
	}
	msg := toolText(r0)
	if !strings.Contains(msg, "renamed to x") {
		t.Errorf("aliased call missing tool output: %q", msg)
	}
	if !strings.Contains(msg, `interpreted "new_name" as "name"`) {
		t.Errorf("aliased call missing alias note: %q", msg)
	}

	// id 2: the canonical name succeeds with no note.
	r1 := resultByID(t, resps, 2)
	if r1["isError"].(bool) || toolText(r1) != "renamed to x" {
		t.Fatalf("canonical call: got %q (isError=%v)", toolText(r1), r1["isError"])
	}

	// id 3: a genuinely unknown key (no alias, not close) is still rejected.
	r2 := resultByID(t, resps, 3)
	if !r2["isError"].(bool) {
		t.Fatalf("genuine unknown should be rejected, got: %s", toolText(r2))
	}
	for _, sub := range []string{`unknown parameter "zzz"`, "valid parameters: name"} {
		if !strings.Contains(toolText(r2), sub) {
			t.Errorf("rejection %q missing %q", toolText(r2), sub)
		}
	}
}

func TestServer_ToolsCall_GuardsMissingRequired(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(strictTool{})
	resps := serveOn(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rename_thing","arguments":{}}}`)
	r := resultByID(t, resps, 1)
	if !r["isError"].(bool) {
		t.Fatal("want isError:true for missing required parameter")
	}
	if msg := toolText(r); !strings.Contains(msg, `missing required parameter "name"`) {
		t.Errorf("message %q missing the required-parameter hint", msg)
	}
}

// editLikeTool mirrors edit_file's nested shape and records the arguments it
// receives, so a test can prove the dispatch layer rewrites legacy keys (both
// top-level and nested) to canonical before Execute is called.
type editLikeTool struct{ gotArgs string }

func (*editLikeTool) Name() string        { return "edit_like" }
func (*editLikeTool) Description() string { return "edit-like tool" }
func (*editLikeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["old_string","new_string"]}}},"required":["file_path","edits"],"additionalProperties":false}`)
}

func (e *editLikeTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	e.gotArgs = string(args)
	return "ok", nil
}

func TestServer_ToolsCall_RewritesLegacyNestedKeys(t *testing.T) {
	tool := &editLikeTool{}
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(tool)
	resps := serveOn(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"edit_like","arguments":{"path":"/f","edits":[{"old_str":"a","new_str":"b"}]}}}`,
	)
	r := resultByID(t, resps, 1)
	if r["isError"].(bool) {
		t.Fatalf("legacy-key call should succeed, got: %s", toolText(r))
	}
	// Execute must have seen canonical keys — top-level and nested.
	got := strings.ReplaceAll(tool.gotArgs, " ", "")
	for _, want := range []string{`"file_path":"/f"`, `"old_string":"a"`, `"new_string":"b"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Execute received %q, missing %q", tool.gotArgs, want)
		}
	}
	// The client is nudged toward the canonical names.
	if !strings.Contains(toolText(r), `interpreted "path" as "file_path"`) {
		t.Errorf("missing alias note: %q", toolText(r))
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
	resps := serve(
		t,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d", len(resps))
	}
}

func TestServer_LargeRequestBelowLimit(t *testing.T) {
	const testMaxMessageBytes = 4 << 20
	req := paddedPingRequest(7, testMaxMessageBytes-1)
	var out bytes.Buffer
	if err := newServer().Serve(context.Background(), strings.NewReader(req+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	dec := json.NewDecoder(&out)
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error for below-limit request: %v", resp["error"])
	}
}

func TestServer_OversizedRequestReturnsJSONRPCError(t *testing.T) {
	const testMaxMessageBytes = 4 << 20
	req := paddedPingRequest(99, testMaxMessageBytes+1024)
	var out bytes.Buffer
	if err := newServer().Serve(context.Background(), strings.NewReader(req+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	dec := json.NewDecoder(&out)
	var resp map[string]any
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"].(float64) != 99 {
		t.Fatalf("oversized response id = %v, want 99", resp["id"])
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || !strings.Contains(errObj["message"].(string), "message exceeds") {
		t.Fatalf("expected message-size error, got %#v", resp)
	}
}

func paddedPingRequest(id, targetBytes int) string {
	prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping","params":{"padding":"`, id)
	suffix := `"}}`
	padLen := max(targetBytes-len(prefix)-len(suffix), 0)
	return prefix + strings.Repeat("x", padLen) + suffix
}

// fakeDeadlineWriter is a writer that, like net.Conn, supports SetWriteDeadline.
// It buffers writes normally and records every deadline set, so a test can
// prove the deadline is applied before a write and cleared after. (The stuck-
// write teardown is covered separately against a real net.Conn via net.Pipe.)
type fakeDeadlineWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	deadlines []time.Time
}

func (f *fakeDeadlineWriter) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines = append(f.deadlines, t)
	return nil
}

func (f *fakeDeadlineWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *fakeDeadlineWriter) recordedDeadlines() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.deadlines...)
}

// On a deadline-capable transport, a happy-path write sets a deadline before
// the write and clears it afterwards, leaving the connection deadline-free
// between replies.
func TestServer_WriteDeadline_SetThenClearedOnSuccess(t *testing.T) {
	s := newServer()
	s.WriteTimeout = 30 * time.Second
	w := &fakeDeadlineWriter{}
	if err := s.Serve(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), w); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if w.buf.Len() == 0 {
		t.Fatal("expected a response to be written")
	}
	ds := w.recordedDeadlines()
	if len(ds) < 2 {
		t.Fatalf("want at least a set+clear deadline pair, got %d: %v", len(ds), ds)
	}
	if ds[0].IsZero() {
		t.Errorf("first deadline should be a future time, got zero")
	}
	if !ds[len(ds)-1].IsZero() {
		t.Errorf("last deadline should be cleared (zero), got %v", ds[len(ds)-1])
	}
}

// On a real net.Conn, a write that stalls past the deadline must fail fast and
// tear the connection down (cancel Serve) instead of wedging on the held write
// mutex forever. net.Pipe is a genuine net.Conn whose Write blocks
// synchronously until the peer reads and which honours SetWriteDeadline, so a
// peer that sends a request but never reads the reply reproduces the wedge
// exactly — and the only way Serve can return is the write-failure cancel path
// (the peer never closes, so there is no EOF).
func TestServer_WriteDeadline_TearsDownOnStuckSocket(t *testing.T) {
	s := newServer()
	s.WriteTimeout = 50 * time.Millisecond
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })

	done := make(chan error, 1)
	go func() { done <- s.Serve(context.Background(), serverConn, serverConn) }()

	// The server reads this (net.Pipe Write unblocks once it does), dispatches
	// the ping, then blocks writing the reply because we never read clientConn.
	if _, err := clientConn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatalf("client write: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want a non-nil error from the torn-down connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return — the stuck socket write wedged the connection")
	}
}

// A transport without SetWriteDeadline (a plain pipe/buffer, as in tests) is
// unaffected by WriteTimeout — the deadline branch is simply skipped.
func TestServer_WriteDeadline_NoDeadlineWriterUnaffected(t *testing.T) {
	s := newServer()
	s.WriteTimeout = time.Nanosecond // would fail instantly if applied to this writer
	var out bytes.Buffer
	if err := s.Serve(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected a response on a non-deadline writer regardless of WriteTimeout")
	}
}
