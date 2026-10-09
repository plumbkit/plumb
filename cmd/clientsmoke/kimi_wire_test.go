//go:build clients_conformance

package clientsmoke

// kimi_wire_test.go — PLAN-413 phases 0 and 1 for Kimi Code: what plumb's
// tool schemas cost per model step, and whether Kimi's progressive disclosure
// (the experimental `tool-select` flag) keeps plumb fully usable.
//
// Kimi Code speaks OpenAI chat completions to a custom provider, so a scripted
// provider can drive it deterministically. With tool-select on AND a model
// declaring `dynamically_loaded_tools`, Kimi keeps MCP schemas out of
// top-level tools[], announces their names in the system context, and loads a
// schema only when the model calls select_tools with its exact name. The
// provider records each request's plumb schema bytes (names and byte counts
// only, never prompts, arguments or results) and walks a script that loads and
// calls one lean and three non-lean plumb tools from different lanes.

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
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// kimiScriptTools are the plumb tools the script calls, in order: read_file is
// lean; workspace_sessions (collaboration), list_memories (memory) and
// copy_file (a guarded mutation) are not.
var kimiScriptTools = []struct {
	base string
	args map[string]any
}{
	{"read_file", map[string]any{"file_path": "read.txt"}},
	{"workspace_sessions", map[string]any{}},
	{"list_memories", map[string]any{}},
	{"copy_file", map[string]any{"from": "read.txt", "to": "copied.txt"}},
}

// kimiProvider is a chat-completions provider that records each request and
// walks the script. Safe for concurrent use.
type kimiProvider struct {
	mu        sync.Mutex
	steps     []wireStep
	err       error
	script    int // index into kimiScriptTools of the next tool to call
	selected  bool
	announced int // plumb tool names announced for select_tools
	waited    int // built-in steps spent waiting for select_tools
	done      bool
}

// errSelectToolsMissing is the script's failure when a plumb tool is absent from
// tools[] and Kimi offers no select_tools to load it.
var errSelectToolsMissing = errors.New("select_tools is not offered")

type kimiChatRequest struct {
	Messages []any `json:"messages"`
	Tools    []any `json:"tools"`
	Stream   bool  `json:"stream"`
}

func (p *kimiProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.Error(w, "accepts POST .../chat/completions only", http.StatusNotFound)
		return
	}
	var req kimiChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	step := len(p.steps)
	p.steps = append(p.steps, measureChatStep(step, req))
	call, text, err := p.next(req)
	if err != nil {
		p.err = err
		text = "clientsmoke error: " + err.Error()
		call = nil
	}
	writeChatResponse(w, req.Stream, step, call, text)
}

// next returns a tool call to make, or final text. A tool missing from
// tools[] is loaded first through select_tools when Kimi offers it.
func (p *kimiProvider) next(req kimiChatRequest) (call map[string]any, text string, err error) {
	if p.done || p.script >= len(kimiScriptTools) {
		p.done = true
		return nil, "kimi wire capture complete", nil
	}
	want := kimiScriptTools[p.script]
	if name, ok := chatToolName(req.Tools, want.base); ok {
		p.script++
		p.selected = false
		return map[string]any{"name": name, "args": want.args}, "", nil
	}
	if !chatHasTool(req.Tools, "select_tools") {
		// Kimi may connect MCP servers and announce their tools a step late.
		// Spend up to two harmless built-in steps before calling it missing.
		if p.waited < 2 && chatHasTool(req.Tools, "Glob") {
			p.waited++
			return map[string]any{"name": "Glob", "args": map[string]any{"pattern": "*.txt"}}, "", nil
		}
		return nil, "", fmt.Errorf("%s is not in tools[] and %w (after %d wait steps)", want.base, errSelectToolsMissing, p.waited)
	}
	if p.selected {
		return nil, "", fmt.Errorf("select_tools did not load %s into tools[]", want.base)
	}
	names := announcedPlumbNames(req.Messages)
	p.announced = max(p.announced, len(names))
	name, ok := matchBase(names, want.base)
	if !ok {
		return nil, "", fmt.Errorf("%s was not announced for select_tools (announced %d plumb tools)", want.base, len(names))
	}
	p.selected = true
	return map[string]any{"name": "select_tools", "args": map[string]any{"names": []string{name}}}, "", nil
}

func (p *kimiProvider) result() ([]wireStep, int, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.steps), p.script, p.announced, p.err
}

// measureChatStep sizes one chat request: plumb function definitions in
// top-level tools[] are direct; plumb definitions anywhere in the messages
// are re-sent ones.
func measureChatStep(step int, req kimiChatRequest) wireStep {
	all, _ := json.Marshal(req.Tools)
	s := wireStep{Step: step, ToolsBytes: len(all)}
	for _, t := range req.Tools {
		if name := chatFuncName(t); strings.Contains(name, "plumb") {
			b, _ := json.Marshal(t)
			s.PlumbDirect++
			s.PlumbDirectBytes += len(b)
		}
	}
	for _, def := range plumbToolDefs(req.Messages) {
		b, _ := json.Marshal(def)
		s.SearchedTools++
		s.SearchedBytes += len(b)
	}
	return s
}

func chatFuncName(t any) string {
	m, _ := t.(map[string]any)
	fn, _ := m["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

func chatHasTool(tools []any, name string) bool {
	return slices.ContainsFunc(tools, func(t any) bool { return chatFuncName(t) == name })
}

// chatToolName finds plumb's tool named base in tools[].
func chatToolName(tools []any, base string) (string, bool) {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if n := chatFuncName(t); strings.Contains(n, "plumb") {
			names = append(names, n)
		}
	}
	return matchBase(names, base)
}

func matchBase(names []string, base string) (string, bool) {
	for _, n := range names {
		if n == base || strings.HasSuffix(n, "__"+base) || strings.HasSuffix(n, "_"+base) && strings.Contains(n, "plumb") {
			return n, true
		}
	}
	return "", false
}

// plumbNameRE matches a plumb tool name as Kimi announces it.
var plumbNameRE = regexp.MustCompile(`[A-Za-z0-9_.-]*plumb[A-Za-z0-9_.-]*`)

// announcedPlumbNames collects the plumb tool names announced in the system
// context for select_tools.
func announcedPlumbNames(messages []any) []string {
	raw, _ := json.Marshal(messages)
	seen := map[string]bool{}
	var out []string
	for _, n := range plumbNameRE.FindAllString(string(raw), -1) {
		if !seen[n] && strings.Contains(n, "__") {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// writeChatResponse answers in the shape the request asked for: an SSE stream
// of chat.completion.chunk events, or one chat.completion object.
func writeChatResponse(w http.ResponseWriter, stream bool, step int, call map[string]any, text string) {
	id := fmt.Sprintf("chatcmpl-clientsmoke-%d", step+1)
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	if call != nil {
		args, _ := json.Marshal(call["args"])
		msg["content"] = nil
		msg["tool_calls"] = []map[string]any{{
			"index": 0, "id": fmt.Sprintf("call_%d", step+1), "type": "function",
			"function": map[string]any{"name": call["name"], "arguments": string(args)},
		}}
		finish = "tool_calls"
	} else {
		msg["content"] = text
	}
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": 0, "model": "clientsmoke-model",
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finishReason any, withUsage bool) {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": 0, "model": "clientsmoke-model",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finishReason}},
		}
		if withUsage {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	chunk(msg, nil, false)
	chunk(map[string]any{}, finish, true)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// kimiMode is one Kimi configuration under test.
type kimiMode struct {
	name       string
	toolSelect bool // [experimental] tool-select
	capability bool // the model declares dynamically_loaded_tools
}

func writeKimiConfig(t *testing.T, kimiHome, providerURL string, m kimiMode) {
	t.Helper()
	caps := `["tool_use"]`
	if m.capability {
		caps = `["tool_use", "dynamically_loaded_tools"]`
	}
	content := fmt.Sprintf(`default_model = "clientsmoke"

[providers.clientsmoke]
type = "openai"
api_key = "clientsmoke"
base_url = %q

[models.clientsmoke]
provider = "clientsmoke"
model = "clientsmoke-model"
max_context_size = 200000
capabilities = %s

[experimental]
tool-select = %t
`, providerURL+"/v1", caps, m.toolSelect)
	if err := os.WriteFile(filepath.Join(kimiHome, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// kimiRun is one Kimi run: the report, how many script tools were called, how
// many plumb names were announced, and whether plumb saw Kimi connect at all.
type kimiRun struct {
	report    wireReport
	called    int
	announced int
	connected bool
}

// runKimiWireCapture runs the script once under m.
func runKimiWireCapture(t *testing.T, m kimiMode) (kimiRun, error) {
	t.Helper()
	kimiPath, err := exec.LookPath("kimi")
	if err != nil {
		t.Skip("kimi (Kimi Code CLI) is not installed")
	}
	tmpHome := mkTmpHome(t)
	kimiHome := filepath.Join(tmpHome, ".kimi-code")
	if err := os.MkdirAll(kimiHome, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := makeBareFixture(t)
	if err := os.WriteFile(filepath.Join(fixture, "read.txt"), []byte("kimi-wire-read-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(conformanceEnv(tmpHome), "KIMI_CODE_HOME="+kimiHome)
	t.Cleanup(func() { stopDaemon(tmpHome) })
	runPlumbSetup(t, env, "setup", "kimi-code")

	provider := &kimiProvider{}
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	writeKimiConfig(t, kimiHome, server.URL, m)

	version, _ := exec.Command(kimiPath, "--version").Output()
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kimiPath, "-p", "Follow the provider's script.", "--output-format", "text")
	cmd.Env = env
	cmd.Dir = fixture
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	steps, called, announced, provErr := provider.result()
	report := wireReport{Client: "kimi-code", Version: strings.TrimSpace(string(version)), Mode: m.name, Steps: steps}
	// A plumb session file proves Kimi completed the MCP handshake with plumb,
	// which separates "Kimi hid plumb's tools" from "plumb never connected".
	_, connected := findClientSession(t, tmpHome)
	if provErr == nil && runErr != nil {
		provErr = fmt.Errorf("kimi exited: %w\nstdout:\n%s\nstderr:\n%s", runErr, truncate(stdout.Bytes(), 3000), truncate(stderr.Bytes(), 3000))
	}
	if provErr == nil && len(steps) == 0 {
		provErr = errors.New("kimi made no model request")
	}
	return kimiRun{report: report, called: called, announced: announced, connected: connected}, provErr
}

// knownKimiDisclosureDefect is the upstream bug that makes tool-select unusable
// headless: with the flag on and a capable model, Kimi 0.38.0 removes every MCP
// schema from tools[] but never registers select_tools (and never announces the
// names), so the model cannot reach plumb at all. Tracked upstream as
// MoonshotAI/kimi-code#2381 (fix PR #2384 unmerged as of 2026-10-09).
const knownKimiDisclosureDefect = "kimi-code#2381"

// TestKimiWireCapture measures each configuration and checks the PLAN-413
// phase 1 claims that hold today. It is a tripwire for the one that does not:
// while the upstream defect stands, tool-select must be observed broken, and
// the day it works this test fails on purpose so that phase 1 is re-run and
// the documented client matrix updated, rather than silently passing.
func TestKimiWireCapture(t *testing.T) {
	modes := []kimiMode{
		{name: "eager", toolSelect: false, capability: true},
		// Phase 1 item 7: the flag without the model capability must degrade to
		// eager, never to zero plumb capability.
		{name: "tool-select-no-capability", toolSelect: true, capability: false},
		{name: "tool-select", toolSelect: true, capability: true},
	}
	var reports []wireReport
	for _, m := range modes {
		run, err := runKimiWireCapture(t, m)
		r, called, announced := run.report, run.called, run.announced
		if m.name == "tool-select" {
			switch {
			case errors.Is(err, errSelectToolsMissing) && !run.connected:
				// Without a handshake this is a setup or harness failure, not
				// the upstream defect: never let it pass as the known bug.
				t.Errorf("tool-select: plumb never connected to Kimi, so this run says nothing about %s: %v", knownKimiDisclosureDefect, err)
			case errors.Is(err, errSelectToolsMissing):
				for _, s := range r.Steps {
					if s.PlumbDirect != 0 {
						t.Errorf("tool-select step %d carried plumb schemas; expected none under %s", s.Step, knownKimiDisclosureDefect)
					}
				}
				t.Logf("client=kimi-code version=%q config=tool-select result=UNREACHABLE (no plumb schema, no select_tools; %s) steps=%d",
					r.Version, knownKimiDisclosureDefect, len(r.Steps))
			case err != nil:
				t.Errorf("tool-select: %v", err)
			default:
				t.Errorf("Kimi %s tool-select now reaches plumb (presentation %s, called %d/%d, %d announced): %s appears fixed. "+
					"Re-run PLAN-413 phase 1, then update this tripwire and the client matrix in docs/token-efficiency.md",
					r.Version, r.presentation(), called, len(kimiScriptTools), announced, knownKimiDisclosureDefect)
			}
			reports = append(reports, r)
			continue
		}
		if err != nil {
			t.Errorf("%s: %v (steps=%d called=%d)", m.name, err, len(r.Steps), called)
			continue
		}
		if called != len(kimiScriptTools) {
			t.Errorf("%s: called %d of %d script tools", m.name, called, len(kimiScriptTools))
		}
		// Both remaining modes must present plumb eagerly; this is also the
		// positive control that the measurement sees plumb schemas at all.
		if r.presentation() != "eager" {
			t.Errorf("%s: no plumb schema in tools[]; expected eager presentation", m.name)
		}
		t.Logf("client=%s version=%q config=%s presentation=%s steps=%d median_plumb_schema_bytes_per_step=%d",
			r.Client, r.Version, r.Mode, r.presentation(), len(r.Steps), r.medianPlumbBytesPerStep())
		reports = append(reports, r)
	}
	if path := os.Getenv("CLIENTSMOKE_KIMI_WIRE_REPORT"); path != "" {
		b, _ := json.MarshalIndent(reports, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Errorf("write wire report: %v", err)
		}
	}
}
