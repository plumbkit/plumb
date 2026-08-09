//go:build clients_conformance

package clientsmoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const conformanceTimeout = 3 * time.Minute

type responsesRequest struct {
	Input []any            `json:"input"`
	Tools []map[string]any `json:"tools"`
}

type toolRef struct {
	name      string
	namespace string
}

type scriptedProvider struct {
	mu        sync.Mutex
	step      int
	fixture   string
	readPath  string
	editPath  string
	tmpHome   string
	toolNames       map[string]toolRef
	advertisedTools int
	err             error
}

func newScriptedProvider(tmpHome, fixture string) *scriptedProvider {
	return &scriptedProvider{
		fixture:   fixture,
		readPath:  filepath.Join(fixture, "read.txt"),
		editPath:  filepath.Join(fixture, "edit.txt"),
		tmpHome:   tmpHome,
		toolNames: make(map[string]toolRef),
	}
}

func (p *scriptedProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.Error(w, "clientsmoke provider accepts POST /v1/responses only", http.StatusNotFound)
		return
	}

	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		http.Error(w, p.err.Error(), http.StatusInternalServerError)
		return
	}

	item, err := p.nextItem(req)
	if err != nil {
		p.err = err
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeResponsesSSE(w, p.step, item)
	p.step++
}

func (p *scriptedProvider) nextItem(req responsesRequest) (map[string]any, error) {
	switch p.step {
	case 0:
		if !requestHasToolType(req.Tools, "tool_search") {
			return nil, errors.New("discovery: Codex request did not expose client-executed tool_search")
		}
		if direct := directPlumbTools(req.Tools); len(direct) > 0 {
			return nil, fmt.Errorf("discovery: Plumb tools leaked onto the direct surface: %v", direct)
		}
		return toolSearchCall("search-1", "plumb session_start read_file edit_file"), nil

	case 1:
		refs := searchedToolRefs(req.Input)
		p.advertisedTools = len(refs)
		for _, ref := range refs {
			for _, base := range []string{"session_start", "read_file", "edit_file"} {
				if strings.HasSuffix(ref.name, "__"+base) || ref.name == base {
					p.toolNames[base] = ref
				}
			}
		}
		var missing []string
		for _, base := range []string{"session_start", "read_file", "edit_file"} {
			if p.toolNames[base].name == "" {
				missing = append(missing, base)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("discovery: tool_search did not return %v (returned %v)", missing, toolRefStrings(refs))
		}
		return functionCall("call-session-start", p.toolNames["session_start"], map[string]any{
			"purpose": "client-conformance",
		}), nil

	case 2:
		body := requestInputText(req.Input)
		if !strings.Contains(body, "Workspace:") {
			return nil, errors.New("invocation: session_start result was not returned to the model")
		}
		if !strings.Contains(body, "Tool profile: full") ||
			!strings.Contains(body, "unverified-deferred-discovery") {
			return nil, errors.New("discovery: session_start did not report the expected conservative Codex profile")
		}
		return functionCall("call-read-success", p.toolNames["read_file"], map[string]any{
			"file_path": p.readPath,
		}), nil

	case 3:
		body := requestInputText(req.Input)
		if !strings.Contains(body, "clientsmoke-read-ok") {
			return nil, errors.New("invocation: successful path-bearing read result was not returned")
		}
		return functionCall("call-edit-refusal", p.toolNames["edit_file"], map[string]any{
			"file_path": p.editPath,
			"edits": []map[string]string{{
				"old_string": "before",
				"new_string": "after",
			}},
		}), nil

	case 4:
		body := requestInputText(req.Input)
		if !strings.Contains(body, "has not been read") {
			return nil, errors.New("recovery: unread edit did not return the expected strict-mode refusal")
		}
		return functionCall("call-read-remediation", p.toolNames["read_file"], map[string]any{
			"file_path": p.editPath,
		}), nil

	case 5:
		body := requestInputText(req.Input)
		mtime := extractHeaderToken(body, "mtime=")
		if mtime == "" {
			return nil, errors.New("recovery: remediation read did not return an mtime token")
		}
		return functionCall("call-edit-remediated", p.toolNames["edit_file"], map[string]any{
			"file_path":      p.editPath,
			"expected_mtime": mtime,
			"edits": []map[string]string{{
				"old_string": "before",
				"new_string": "after",
			}},
		}), nil

	case 6:
		body := requestInputText(req.Input)
		if !strings.Contains(body, "applied 1 edit") {
			return nil, errors.New("recovery: edit did not succeed after the advertised read remediation")
		}
		return functionCall("call-read-before-reconnect", p.toolNames["read_file"], map[string]any{
			"file_path": p.editPath,
		}), nil

	case 7:
		body := requestInputText(req.Input)
		mtime := extractHeaderToken(body, "mtime=")
		if mtime == "" || !strings.Contains(body, "after") {
			return nil, errors.New("reconnect: pre-restart read did not return the edited content and mtime")
		}
		stopDaemon(p.tmpHome)
		return functionCall("call-edit-after-reconnect", p.toolNames["edit_file"], map[string]any{
			"file_path":      p.editPath,
			"expected_mtime": mtime,
			"edits": []map[string]string{{
				"old_string": "after",
				"new_string": "after-reconnect",
			}},
		}), nil

	case 8:
		body := requestInputText(req.Input)
		if !strings.Contains(body, "applied 1 edit") {
			return nil, errors.New("reconnect: edit did not succeed from replayed pre-restart read state")
		}
		return assistantMessage("clientsmoke deterministic scenario complete"), nil

	default:
		return nil, fmt.Errorf("provider received unexpected request %d", p.step+1)
	}
}

func (p *scriptedProvider) result() (step int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step, p.err
}

func (p *scriptedProvider) advertisedToolCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.advertisedTools
}

func writeResponsesSSE(w http.ResponseWriter, step int, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	responseID := fmt.Sprintf("resp_clientsmoke_%d", step+1)
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": responseID}},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{
			"id": responseID,
			"usage": map[string]any{
				"input_tokens":          0,
				"input_tokens_details":  nil,
				"output_tokens":         0,
				"output_tokens_details": nil,
				"total_tokens":          0,
			},
		}},
	}
	for _, event := range events {
		payload, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], payload)
	}
}

func toolSearchCall(id, query string) map[string]any {
	return map[string]any{
		"type":      "tool_search_call",
		"id":        id,
		"call_id":   id,
		"execution": "client",
		"status":    "completed",
		"arguments": map[string]any{"query": query, "limit": 100},
	}
}

func functionCall(callID string, ref toolRef, arguments map[string]any) map[string]any {
	encoded, _ := json.Marshal(arguments)
	item := map[string]any{
		"type":      "function_call",
		"id":        "item-" + callID,
		"call_id":   callID,
		"name":      ref.name,
		"arguments": string(encoded),
		"status":    "completed",
	}
	if ref.namespace != "" {
		item["namespace"] = ref.namespace
	}
	return item
}

func assistantMessage(text string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     "message-clientsmoke",
		"role":   "assistant",
		"status": "completed",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

func requestHasToolType(tools []map[string]any, want string) bool {
	for _, tool := range tools {
		if tool["type"] == want {
			return true
		}
	}
	return false
}

func directPlumbTools(tools []map[string]any) []string {
	var names []string
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if strings.Contains(name, "plumb") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func searchedToolRefs(input []any) []toolRef {
	seen := make(map[toolRef]bool)
	walkJSON(input, func(value map[string]any) {
		name, _ := value["name"].(string)
		tools, hasTools := value["tools"].([]any)
		if hasTools && strings.Contains(name, "plumb") {
			for _, rawTool := range tools {
				tool, ok := rawTool.(map[string]any)
				if !ok {
					continue
				}
				childName, _ := tool["name"].(string)
				if childName != "" {
					seen[toolRef{name: childName, namespace: name}] = true
				}
			}
			return
		}
		if strings.Contains(name, "plumb") {
			seen[toolRef{name: name}] = true
		}
	})
	refs := make([]toolRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].namespace != refs[j].namespace {
			return refs[i].namespace < refs[j].namespace
		}
		return refs[i].name < refs[j].name
	})
	return refs
}

func toolRefStrings(refs []toolRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.namespace == "" {
			names = append(names, ref.name)
		} else {
			names = append(names, ref.namespace+"."+ref.name)
		}
	}
	return names
}

func walkJSON(value any, visit func(map[string]any)) {
	switch v := value.(type) {
	case map[string]any:
		visit(v)
		for _, child := range v {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range v {
			walkJSON(child, visit)
		}
	}
}

func requestInputText(input []any) string {
	encoded, _ := json.Marshal(input)
	return string(encoded)
}

func extractHeaderToken(body, prefix string) string {
	at := strings.LastIndex(body, prefix)
	if at < 0 {
		return ""
	}
	value := body[at+len(prefix):]
	if end := strings.IndexAny(value, " \\n\\r\\t"); end >= 0 {
		value = value[:end]
	}
	return strings.Trim(value, "\\\"")
}

func writeCodexConformanceProfile(t *testing.T, tmpHome, providerURL string) {
	t.Helper()
	dir := filepath.Join(tmpHome, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal("create Codex profile directory:", err)
	}
	content := fmt.Sprintf(`model = "gpt-5.5"
model_provider = "clientsmoke"

[model_providers.clientsmoke]
name = "clientsmoke deterministic provider"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 10000

[mcp_servers.plumb]
omit_tools_from = ["direct"]
`, providerURL+"/v1")
	if err := os.WriteFile(filepath.Join(dir, "clientsmoke.config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal("write Codex conformance profile:", err)
	}
}

func TestCodexDeterministicConformance(t *testing.T) {
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed; run scripts/install-clients.sh")
	}

	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	if err := os.WriteFile(filepath.Join(fixture, "read.txt"), []byte("clientsmoke-read-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "edit.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fixture, ".plumb"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, ".plumb", "config.toml"), []byte("[edits]\nstrict = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := isolatedEnv(tmpHome, "PLUMB_STRICT_EDITS=1")
	t.Cleanup(func() { stopDaemon(tmpHome) })
	runPlumbSetup(t, env, "setup", "codex")

	provider := newScriptedProvider(tmpHome, fixture)
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	writeCodexConformanceProfile(t, tmpHome, server.URL)

	versionCmd := exec.Command(codexPath, "--version")
	versionCmd.Env = env
	versionOut, versionErr := versionCmd.CombinedOutput()
	if versionErr != nil {
		t.Fatalf("codex --version: %v\n%s", versionErr, versionOut)
	}

	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath,
		"-p", "clientsmoke",
		"exec",
		"--ephemeral",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
		"Execute the deterministic conformance scenario exactly as directed by the provider.",
	)
	cmd.Env = env
	cmd.Dir = fixture
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	steps, providerErr := provider.result()
	if providerErr != nil {
		t.Fatalf("mcp_failure: steps=%d err=%v\nstdout:\n%s\nstderr:\n%s", steps, providerErr, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}
	if ctx.Err() != nil {
		t.Fatalf("client_timeout: after %s; stdout:\n%s\nstderr:\n%s", conformanceTimeout, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}
	if runErr != nil {
		t.Fatalf("client_exit: %v\nstdout:\n%s\nstderr:\n%s", runErr, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}
	if steps == 0 {
		t.Fatalf("no_tool_invocation: provider received no model request; stdout:\n%s\nstderr:\n%s", truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}
	if got := strings.TrimSpace(stdout.String()); !strings.Contains(got, "clientsmoke deterministic scenario complete") {
		t.Fatalf("no_tool_invocation: client never completed the scripted tool scenario; stdout:\n%s\nstderr:\n%s", truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}
	if steps != 9 {
		t.Fatalf("scenario_incomplete: steps=%d want=9\nstdout:\n%s\nstderr:\n%s", steps, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
	}

	edited, err := os.ReadFile(filepath.Join(fixture, "edit.txt"))
	if err != nil {
		t.Fatal("read edited fixture:", err)
	}
	if string(edited) != "after-reconnect\n" {
		t.Fatalf("edited fixture = %q, want %q", edited, "after-reconnect\\n")
	}

	sess, ok := findClientSession(t, tmpHome)
	if !ok {
		t.Fatal("session identity: plumb recorded no Codex session")
	}
	if !strings.HasPrefix(strings.ToLower(sess.ClientName), "codex") {
		t.Fatalf("session client_name = %q, want Codex", sess.ClientName)
	}
	if sess.ClientVersion == "" {
		t.Fatal("session client_version is empty")
	}
	if sess.Folder != fixture {
		t.Fatalf("session folder = %q, want %q", sess.Folder, fixture)
	}

	n, toolNames := pollToolCalls(t, tmpHome, 8*time.Second)
	if n < 7 {
		t.Fatalf("missing_stats: got %d calls [%s], want at least 7 scenario calls", n, toolNames)
	}
	t.Logf("PASS client=codex binary=%q mcp_client=%q os=%s arch=%s profile=full reason=unverified-deferred-discovery advertised_tools=%d calls=%d tools=%s",
		strings.TrimSpace(string(versionOut)), sess.ClientVersion, runtime.GOOS, runtime.GOARCH, provider.advertisedToolCount(), n, toolNames)
}
