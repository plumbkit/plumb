package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// failingTool returns a fixed error, so a test can choose whether the failure
// carries a classification.
type failingTool struct {
	name string
	err  error
}

func (f *failingTool) Name() string        { return f.name }
func (f *failingTool) Description() string { return "always fails" }
func (f *failingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (f *failingTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", f.err
}

// callFailing registers t and drives one tools/call against it.
func callFailing(t *testing.T, tool mcp.Tool) map[string]any {
	t.Helper()
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(tool)
	resps := serveOn(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool.Name()+`","arguments":{}}}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	return resultByID(t, resps, 1)
}

// mustJSON re-encodes v so a nested `any` tree can be compared against a JSON
// literal without depending on Go map ordering.
func mustJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return out
}

// TestToolsCall_ClassifiedErrorEmitsMeta pins the whole envelope: the exact
// `_meta` object, and — just as importantly — that `content` and `isError` are
// untouched by its presence.
func TestToolsCall_ClassifiedErrorEmitsMeta(t *testing.T) {
	const text = "git commit: exit code 1\nstderr:\nhook: lint failed"
	failure := toolerror.New(toolerror.KindGitCommandFailed, errors.New(text),
		toolerror.Remediation{Class: toolerror.ClassInspectOutput, Reason: "Read the captured output."},
		toolerror.WithDetail("exit_code", "1"),
		toolerror.WithDetail("subcommand", "commit"),
	)
	result := callFailing(t, &failingTool{name: "git", err: failure})

	if isErr, _ := result["isError"].(bool); !isErr {
		t.Error("isError must stay true for a failed call")
	}
	if got, want := toolText(result), "error: "+text; got != want {
		t.Errorf("content text = %q, want %q", got, want)
	}

	want := decodeJSON(t, `{
		"dev.plumbkit/error": {
			"kind": "git_command_failed",
			"operation": "git",
			"retryable": false,
			"remediation": {"class": "inspect_output", "reason": "Read the captured output."},
			"details": {"exit_code": "1", "subcommand": "commit"}
		}
	}`)
	got := mustJSON(t, result["_meta"])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("_meta mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestToolsCall_MetaCarriesToolAndRetryable covers the other envelope shape:
// a retryable refusal naming the tool to reach for, and no details.
func TestToolsCall_MetaCarriesToolAndRetryable(t *testing.T) {
	failure := toolerror.Wrap(errors.New("write_file: stale"),
		toolerror.KindUnreadOrStale, toolerror.ClassReRead,
		toolerror.WithTool("read_file"), toolerror.WithReason("Re-read it."))
	result := callFailing(t, &failingTool{name: "write_file", err: failure})

	want := decodeJSON(t, `{
		"dev.plumbkit/error": {
			"kind": "unread_or_stale",
			"operation": "write_file",
			"retryable": true,
			"remediation": {"class": "re_read", "tool": "read_file", "reason": "Re-read it."}
		}
	}`)
	got := mustJSON(t, result["_meta"])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("_meta mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

// TestToolsCall_UnclassifiedErrorHasNoMeta is the absence guarantee: a client
// may read a missing key as "plumb has no structured claim", so an
// unclassified failure must not emit a hollow envelope.
func TestToolsCall_UnclassifiedErrorHasNoMeta(t *testing.T) {
	result := callFailing(t, &failingTool{name: "mystery", err: errors.New("something went wrong")})

	if _, present := result["_meta"]; present {
		t.Errorf("_meta emitted for an unclassified failure: %v", result["_meta"])
	}
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Error("isError must stay true")
	}
	if got, want := toolText(result), "error: something went wrong"; got != want {
		t.Errorf("content text = %q, want %q", got, want)
	}
}

// TestToolsCall_SuccessHasNoMeta keeps the successful payload byte-identical to
// what it was before the envelope existed.
func TestToolsCall_SuccessHasNoMeta(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	result := resultByID(t, resps, 1)
	if _, present := result["_meta"]; present {
		t.Errorf("_meta emitted on a successful call: %v", result["_meta"])
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Error("isError must be false on success")
	}
}

// TestToolsCall_ArgumentGuardFailureIsClassified proves the classification
// survives the alias/validation path, which fails before the tool runs but
// still returns through the tool-result path.
func TestToolsCall_ArgumentGuardFailureIsClassified(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(strictTool{})
	resps := serveOn(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rename_thing","arguments":{"zzz":"x"}}}`)
	result := resultByID(t, resps, 1)

	meta, ok := result["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("no _meta on an argument-guard failure: %v", result)
	}
	env, _ := meta["dev.plumbkit/error"].(map[string]any)
	if env["kind"] != "invalid_arguments" {
		t.Errorf("kind = %v, want invalid_arguments", env["kind"])
	}
	if env["operation"] != "rename_thing" {
		t.Errorf("operation = %v, want rename_thing", env["operation"])
	}
	// fix_arguments is retryable: the agent corrects the call and re-issues it.
	if env["retryable"] != true {
		t.Errorf("retryable = %v, want true", env["retryable"])
	}
	// The rendered text must still be the guard's own sentence.
	if msg := toolText(result); !strings.Contains(msg, `unknown parameter "zzz"`) {
		t.Errorf("content text lost the guard's wording: %q", msg)
	}
}

// jsonRPCError pulls the error object out of a raw response.
func jsonRPCError(t *testing.T, resps []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, r := range resps {
		if rid, ok := r["id"].(float64); ok && rid == id {
			e, isObj := r["error"].(map[string]any)
			if !isObj {
				t.Fatalf("response %v carries no error object: %v", id, r)
			}
			return e
		}
	}
	t.Fatalf("no response with id %v", id)
	return nil
}

func TestJSONRPCError_UnknownToolCarriesData(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)
	e := jsonRPCError(t, resps, 5)

	if code, _ := e["code"].(float64); code != -32601 {
		t.Errorf("code = %v, want -32601", e["code"])
	}
	msg, _ := e["message"].(string)
	if !strings.HasPrefix(msg, "unknown tool: nope") {
		t.Errorf("message = %q, want it to start with %q", msg, "unknown tool: nope")
	}

	want := decodeJSON(t, `{
		"kind": "invalid_arguments",
		"operation": "nope",
		"retryable": true,
		"remediation": {"class": "fix_arguments", "reason": "Correct the call's arguments and retry."}
	}`)
	if got := mustJSON(t, e["data"]); !reflect.DeepEqual(got, want) {
		t.Errorf("error.data mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestJSONRPCError_MalformedParamsCarriesData(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":"not-an-object"}`)
	e := jsonRPCError(t, resps, 6)

	if code, _ := e["code"].(float64); code != -32602 {
		t.Errorf("code = %v, want -32602", e["code"])
	}
	msg, _ := e["message"].(string)
	if !strings.HasPrefix(msg, "invalid params: ") {
		t.Errorf("message = %q, want it to start with %q", msg, "invalid params: ")
	}

	data, ok := e["data"].(map[string]any)
	if !ok {
		t.Fatalf("no error.data on a malformed tools/call: %v", e)
	}
	if data["kind"] != "invalid_arguments" {
		t.Errorf("kind = %v, want invalid_arguments", data["kind"])
	}
	// No tool was resolved, so there is no operation to name.
	if _, present := data["operation"]; present {
		t.Errorf("operation emitted with no resolved tool: %v", data["operation"])
	}
}

// TestJSONRPCError_OtherRejectionsCarryNoData keeps every pre-existing errResp
// caller byte-identical: only the two classified tools/call rejections gained
// a data field.
func TestJSONRPCError_OtherRejectionsCarryNoData(t *testing.T) {
	resps := serve(t, `{"jsonrpc":"2.0","id":7,"method":"no/such/method"}`)
	e := jsonRPCError(t, resps, 7)
	if _, present := e["data"]; present {
		t.Errorf("data emitted on an unrelated rejection: %v", e["data"])
	}
	if code, _ := e["code"].(float64); code != -32601 {
		t.Errorf("code = %v, want -32601", e["code"])
	}
}
