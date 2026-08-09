//go:build clients_conformance

package clientsmoke

import (
	"bytes"
	"context"
	"encoding/json"
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

type chatRequest struct {
	Messages []any            `json:"messages"`
	Tools    []map[string]any `json:"tools"`
}

type chatScriptedProvider struct {
	mu              sync.Mutex
	step            int
	scenario        *conformanceScenario
	toolNames       map[string]toolRef
	advertisedTools int
	err             error
}

func newChatScriptedProvider(tmpHome, fixture string) *chatScriptedProvider {
	return &chatScriptedProvider{
		scenario:  newConformanceScenario(tmpHome, fixture),
		toolNames: make(map[string]toolRef),
	}
}

func (p *chatScriptedProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.Error(w, "clientsmoke provider accepts POST /v1/chat/completions only", http.StatusNotFound)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(req.Tools) == 0 {
		writeChatSSE(w, -1, assistantMessage("clientsmoke session"))
		return
	}
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
	writeChatSSE(w, p.step, item)
	p.step++
}

func (p *chatScriptedProvider) nextItem(req chatRequest) (map[string]any, error) {
	body := requestInputText(req.Messages)
	if p.step == 0 {
		refs := directChatToolRefs(req.Tools)
		p.advertisedTools = len(refs)
		for _, ref := range refs {
			for _, base := range conformanceRequiredTools() {
				if strings.HasSuffix(ref.name, base) {
					p.toolNames[base] = ref
				}
			}
		}
		var missing []string
		for _, base := range conformanceRequiredTools() {
			if p.toolNames[base].name == "" {
				missing = append(missing, base)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("discovery: client did not advertise %v (returned %v)", missing, toolRefStrings(refs))
		}
		body = ""
	}
	return p.nextScenarioItem(body)
}

func (p *chatScriptedProvider) nextScenarioItem(body string) (map[string]any, error) {
	action, err := p.scenario.next(body)
	if err != nil {
		return nil, err
	}
	if action.complete {
		return assistantMessage("clientsmoke deterministic scenario complete"), nil
	}
	ref := p.toolNames[action.stage.tool]
	if ref.name == "" {
		return nil, fmt.Errorf("%s: tool %s was not advertised", action.stage.name, action.stage.tool)
	}
	callID := "chat-" + strings.ReplaceAll(action.stage.name, "_", "-")
	return functionCall(callID, ref, action.arguments), nil
}

func (p *chatScriptedProvider) result() (step int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step, p.err
}

func (p *chatScriptedProvider) advertisedToolCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.advertisedTools
}

func directChatToolRefs(tools []map[string]any) []toolRef {
	seen := make(map[string]bool)
	var refs []toolRef
	for _, tool := range tools {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if name == "" || !strings.Contains(name, "plumb") || seen[name] {
			continue
		}
		seen[name] = true
		refs = append(refs, toolRef{name: name})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func writeChatSSE(w http.ResponseWriter, step int, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	id := fmt.Sprintf("chatcmpl_clientsmoke_%d", step+1)
	writeChunk := func(delta map[string]any, finish any) {
		payload, _ := json.Marshal(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "clientsmoke",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}

	if item["type"] == "function_call" {
		writeChunk(map[string]any{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"index": 0,
				"id":    item["call_id"],
				"type":  "function",
				"function": map[string]any{
					"name":      item["name"],
					"arguments": item["arguments"],
				},
			}},
		}, nil)
		writeChunk(map[string]any{}, "tool_calls")
	} else {
		content := ""
		if parts, ok := item["content"].([]map[string]any); ok && len(parts) > 0 {
			content, _ = parts[0]["text"].(string)
		}
		writeChunk(map[string]any{"role": "assistant", "content": content}, nil)
		writeChunk(map[string]any{}, "stop")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeOpenCodeConformanceConfig(t *testing.T, tmpHome, providerURL string) {
	t.Helper()
	path := filepath.Join(tmpHome, ".config", "opencode", "opencode.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read OpenCode setup config:", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal("decode OpenCode setup config:", err)
	}
	cfg["enabled_providers"] = []string{"clientsmoke"}
	cfg["provider"] = map[string]any{
		"clientsmoke": map[string]any{
			"npm":  "@ai-sdk/openai-compatible",
			"name": "clientsmoke deterministic provider",
			"options": map[string]any{
				"baseURL": providerURL + "/v1",
				"apiKey":  "clientsmoke-not-a-credential",
			},
			"models": map[string]any{
				"clientsmoke": map[string]any{
					"name": "clientsmoke",
					"limit": map[string]any{
						"context": 32000,
						"output":  4096,
					},
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal("encode OpenCode conformance config:", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal("write OpenCode conformance config:", err)
	}
}

func TestOpenCodeDeterministicConformance(t *testing.T) {
	opencodePath, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal("opencode is required; run scripts/install-clients.sh")
	}

	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	if err := os.WriteFile(filepath.Join(fixture, "read.txt"), []byte("clientsmoke-read-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "edit.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, ".plumb", "config.toml"), []byte("[edits]\nstrict = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := conformanceEnv(tmpHome, "PLUMB_STRICT_EDITS=1", "OPENCODE_DISABLE_AUTOUPDATE=1")
	t.Cleanup(func() { stopDaemon(tmpHome) })
	runPlumbSetup(t, env, "setup", "opencode")

	provider := newChatScriptedProvider(tmpHome, fixture)
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	writeOpenCodeConformanceConfig(t, tmpHome, server.URL)

	versionCmd := exec.Command(opencodePath, "--version")
	versionCmd.Env = env
	versionOut, versionErr := versionCmd.CombinedOutput()
	if versionErr != nil {
		t.Fatalf("opencode --version: %v\n%s", versionErr, versionOut)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, opencodePath,
		"run",
		"--pure",
		"--print-logs",
		"--log-level", "DEBUG",
		"--auto",
		"--model", "clientsmoke/clientsmoke",
		"--format", "json",
		"--dir", fixture,
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
		t.Fatalf("mcp_failure: steps=%d err=%v\nstdout:\n%s\nstderr:\n%s", steps, providerErr, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 8000))
	}
	if ctx.Err() != nil {
		t.Fatalf("client_timeout: after %s; stdout:\n%s\nstderr:\n%s", 45*time.Second, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 8000))
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
	if steps != 8 {
		t.Fatalf("scenario_incomplete: steps=%d want=8\nstdout:\n%s\nstderr:\n%s", steps, truncate(stdout.Bytes(), 4000), truncate(stderr.Bytes(), 4000))
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
		t.Fatal("session identity: plumb recorded no OpenCode session")
	}
	if !strings.Contains(strings.ToLower(sess.ClientName), "opencode") {
		t.Fatalf("session client_name = %q, want OpenCode", sess.ClientName)
	}
	if sess.ClientVersion == "" {
		t.Fatal("session client_version is empty")
	}
	if sess.Folder != fixture {
		t.Fatalf("session folder = %q, want %q", sess.Folder, fixture)
	}

	n, toolNames := pollToolCallsAtLeast(t, tmpHome, 7, 8*time.Second)
	if n < 7 {
		t.Fatalf("missing_stats: got %d calls [%s], want at least 7 scenario calls", n, toolNames)
	}
	t.Logf("PASS client=opencode binary=%q mcp_client=%q os=%s arch=%s profile=full reason=direct-tool-surface advertised_tools=%d calls=%d tools=%s",
		strings.TrimSpace(string(versionOut)), sess.ClientVersion, runtime.GOOS, runtime.GOARCH, provider.advertisedToolCount(), n, toolNames)
}
