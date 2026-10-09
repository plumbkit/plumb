//go:build clients_conformance

package clientsmoke

// wire_capture_test.go — PLAN-413 phase 0: what plumb's tool schemas actually
// cost a client per model step, measured on the wire instead of inferred from
// the advertised catalogue.
//
// session_start can only report the size of the catalogue plumb ADVERTISES;
// what reaches the model each step depends on how the client presents MCP
// tools. A scripted provider sees every request the client makes, so it can
// count, per step, the bytes of plumb tool schemas in the request's top-level
// tools[] and the bytes a deferred client re-injects after a tool search. No
// prompt text, arguments or results are recorded: only names and byte counts.
//
// Each mode drives the same short script (discover read_file, call it, finish)
// and asserts the presentation it claims:
//
//   - deferred: `omit_tools_from = ["direct"]`, what `plumb setup codex`
//     writes. No plumb schema may sit in top-level tools[] on ANY step.
//   - eager: no omission. Whether the client then sends plumb's schemas
//     eagerly is MEASURED and reported, not assumed: Codex 0.161 defers MCP
//     tools regardless.
//   - lean: PLUMB_TOOLS_PROFILE=lean, the explicit fallback. It can never cost
//     more than the full profile.
//
// Set CLIENTSMOKE_WIRE_REPORT=<path> to also write the measurements as JSON.

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
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// wireStep is one model request as the provider saw it.
type wireStep struct {
	Step int `json:"step"`
	// ToolsBytes is the whole top-level tools[] array, client tools included.
	ToolsBytes int `json:"tools_bytes"`
	// PlumbDirect counts plumb tools in the top-level tools[] and their bytes:
	// what the model is sent on this step before any discovery.
	PlumbDirect      int `json:"plumb_direct"`
	PlumbDirectBytes int `json:"plumb_direct_bytes"`
	// SearchedBytes is the bytes of plumb tool definitions present in the
	// request INPUT (a tool_search result the client re-sends), and how many.
	SearchedBytes int `json:"searched_bytes"`
	SearchedTools int `json:"searched_tools"`
}

// wireReport is one mode's measurement.
type wireReport struct {
	Client  string     `json:"client"`
	Version string     `json:"version"`
	Mode    string     `json:"mode"`
	Steps   []wireStep `json:"steps"`
}

// presentation reports what the client actually did with plumb's schemas:
// "eager" when any step carried them in top-level tools[], otherwise
// "deferred".
func (r wireReport) presentation() string {
	for _, s := range r.Steps {
		if s.PlumbDirect > 0 {
			return "eager"
		}
	}
	return "deferred"
}

// plumbBytesSeen is the total plumb schema bytes over all steps: zero means
// the measurement never saw a plumb tool at all.
func (r wireReport) plumbBytesSeen() int {
	n := 0
	for _, s := range r.Steps {
		n += s.PlumbDirectBytes + s.SearchedBytes
	}
	return n
}

// medianPlumbBytesPerStep is the median over steps of the plumb schema bytes
// the model received on that step, direct and re-injected together.
func (r wireReport) medianPlumbBytesPerStep() int {
	if len(r.Steps) == 0 {
		return 0
	}
	vals := make([]int, 0, len(r.Steps))
	for _, s := range r.Steps {
		vals = append(vals, s.PlumbDirectBytes+s.SearchedBytes)
	}
	sort.Ints(vals)
	return vals[len(vals)/2]
}

// measuringProvider is a Responses-API provider that records each request and
// walks a three-step script. Safe for concurrent use: the client may retry.
type measuringProvider struct {
	mu    sync.Mutex
	steps []wireStep
	err   error
	ref   toolRef
}

func (p *measuringProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.Error(w, "accepts POST /v1/responses only", http.StatusNotFound)
		return
	}
	var req struct {
		Input []any `json:"input"`
		Tools []any `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	step := len(p.steps)
	p.steps = append(p.steps, measureStep(step, req.Tools, req.Input))
	item, err := p.next(step, req.Tools, req.Input)
	if err != nil {
		p.err = err
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeResponsesSSE(w, step, item)
}

// next is the script: discover read_file (by tool search when the client
// defers, from tools[] when it does not), call it, then finish.
func (p *measuringProvider) next(step int, tools, input []any) (map[string]any, error) {
	switch step {
	case 0:
		if ref, ok := findPlumbTool(tools, "read_file"); ok {
			p.ref = ref
			return functionCall("call-read", ref, map[string]any{"file_path": "read.txt"}), nil
		}
		if !slices.ContainsFunc(tools, func(t any) bool { m, _ := t.(map[string]any); return m["type"] == "tool_search" }) {
			return nil, errors.New("step 0: no plumb read_file in tools[] and no tool_search to find it")
		}
		return toolSearchCall("search-1", "read_file"), nil
	case 1:
		if p.ref.name != "" {
			return assistantMessage("wire capture complete"), nil
		}
		ref, ok := findPlumbTool(input, "read_file")
		if !ok {
			return nil, errors.New("step 1: tool_search returned no plumb read_file")
		}
		p.ref = ref
		return functionCall("call-read", ref, map[string]any{"file_path": "read.txt"}), nil
	default:
		return assistantMessage("wire capture complete"), nil
	}
}

func (p *measuringProvider) result() ([]wireStep, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.steps), p.err
}

// measureStep sizes one request. A plumb tool is any tool definition whose
// name or enclosing namespace names plumb.
func measureStep(step int, tools, input []any) wireStep {
	all, _ := json.Marshal(tools)
	s := wireStep{Step: step, ToolsBytes: len(all)}
	for _, def := range plumbToolDefs(tools) {
		b, _ := json.Marshal(def)
		s.PlumbDirect++
		s.PlumbDirectBytes += len(b)
	}
	for _, def := range plumbToolDefs(input) {
		b, _ := json.Marshal(def)
		s.SearchedTools++
		s.SearchedBytes += len(b)
	}
	return s
}

// plumbToolDefs returns every tool definition under v that belongs to plumb:
// a definition named for plumb, or one nested in a plumb namespace's tools.
// A definition is an object with a name and a parameters or input schema.
func plumbToolDefs(v any) []map[string]any {
	var out []map[string]any
	walkJSON(v, func(m map[string]any) {
		name, _ := m["name"].(string)
		if !strings.Contains(name, "plumb") {
			return
		}
		if children, ok := m["tools"].([]any); ok {
			for _, c := range children {
				if def, ok := c.(map[string]any); ok && isToolDef(def) {
					out = append(out, def)
				}
			}
			return
		}
		if isToolDef(m) {
			out = append(out, m)
		}
	})
	return out
}

func isToolDef(m map[string]any) bool {
	_, hasParams := m["parameters"]
	_, hasSchema := m["input_schema"]
	return hasParams || hasSchema
}

// findPlumbTool finds plumb's tool named base anywhere under v.
func findPlumbTool(v any, base string) (toolRef, bool) {
	for _, ref := range searchedToolRefs([]any{v}) {
		if ref.name == base || strings.HasSuffix(ref.name, "__"+base) {
			return ref, true
		}
	}
	return toolRef{}, false
}

func writeCodexWireProfile(t *testing.T, tmpHome, providerURL, mode string) {
	t.Helper()
	omit := ""
	if mode == "deferred" {
		omit = "omit_tools_from = [\"direct\"]\n"
	}
	env := ""
	if mode == "lean" {
		env = "\n[mcp_servers.plumb.env]\nPLUMB_TOOLS_PROFILE = \"lean\"\n"
	}
	content := fmt.Sprintf(`model = "gpt-5.5"
model_provider = "clientsmoke"

[model_providers.clientsmoke]
name = "clientsmoke wire capture"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 10000

[mcp_servers.plumb]
%s%s`, providerURL+"/v1", omit, env)
	dir := filepath.Join(tmpHome, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clientsmoke.config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCodexWireCapture runs the script once under mode and returns the report.
func runCodexWireCapture(t *testing.T, mode string) wireReport {
	t.Helper()
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("codex is required; run scripts/install-clients.sh")
	}
	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	if err := os.WriteFile(filepath.Join(fixture, "read.txt"), []byte("wire-capture-read-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := conformanceEnv(tmpHome)
	t.Cleanup(func() { stopDaemon(tmpHome) })
	runPlumbSetup(t, env, "setup", "codex")

	provider := &measuringProvider{}
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	writeCodexWireProfile(t, tmpHome, server.URL, mode)

	version, _ := exec.Command(codexPath, "--version").Output()
	ctx, cancel := context.WithTimeout(context.Background(), conformanceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, "-p", "clientsmoke", "exec", "--ephemeral",
		"--dangerously-bypass-approvals-and-sandbox", "--color", "never",
		"Follow the provider's script.")
	cmd.Env = env
	cmd.Dir = fixture
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	steps, provErr := provider.result()
	if provErr != nil || runErr != nil || !strings.Contains(stdout.String(), "wire capture complete") {
		t.Fatalf("%s: provider=%v run=%v steps=%d\nstdout:\n%s\nstderr:\n%s", mode, provErr, runErr, len(steps),
			truncate(stdout.Bytes(), 3000), truncate(stderr.Bytes(), 3000))
	}
	return wireReport{Client: "codex", Version: strings.TrimSpace(string(version)), Mode: mode, Steps: steps}
}

// TestCodexWireCapture measures what each configuration sends and pins the
// claims plumb's setup makes about Codex. It does not assume Codex's
// presentation: it records it. Codex 0.161 always defers MCP tools behind
// tool_search (its tool_search_always_defer_mcp_tools feature is fixed on), so
// "eager" here is only the configuration WITHOUT omit_tools_from. Whether it
// is actually eager is measured and reported, never asserted.
func TestCodexWireCapture(t *testing.T) {
	reports := []wireReport{
		runCodexWireCapture(t, "eager"),
		runCodexWireCapture(t, "lean"),
		runCodexWireCapture(t, "deferred"),
	}
	byMode := map[string]wireReport{}
	for _, r := range reports {
		byMode[r.Mode] = r
		// Positive control: every run must see SOME plumb schema bytes, or a
		// zero below is the measurement failing rather than the client deferring.
		if r.plumbBytesSeen() == 0 {
			t.Errorf("%s: no plumb tool schema seen on any step; the measurement is blind", r.Mode)
		}
	}
	// The claim `plumb setup codex` makes: omit_tools_from=["direct"] keeps
	// plumb's schemas out of the model's top-level tools[] on every step.
	for _, s := range byMode["deferred"].Steps {
		if s.PlumbDirect != 0 {
			t.Errorf("deferred step %d carried %d plumb schemas (%d bytes) in top-level tools[]", s.Step, s.PlumbDirect, s.PlumbDirectBytes)
		}
	}
	// lean advertises a subset, so it can never cost more than the full set.
	if l, e := byMode["lean"].medianPlumbBytesPerStep(), byMode["eager"].medianPlumbBytesPerStep(); l > e {
		t.Errorf("lean median %d bytes/step exceeds the full profile's %d", l, e)
	}

	for _, r := range reports {
		t.Logf("client=%s version=%q config=%s presentation=%s steps=%d median_plumb_schema_bytes_per_step=%d direct_bytes_step0=%d",
			r.Client, r.Version, r.Mode, r.presentation(), len(r.Steps), r.medianPlumbBytesPerStep(), r.Steps[0].PlumbDirectBytes)
	}
	if path := os.Getenv("CLIENTSMOKE_WIRE_REPORT"); path != "" {
		b, _ := json.MarshalIndent(reports, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Errorf("write wire report: %v", err)
		}
	}
}

// TestMeasureStepCountsDirectPlumbShapes is the positive control for the
// direct-path count. The wire tests only ever see Codex defer, so on their own
// they could not tell "no plumb schema in tools[]" from "the measurement does
// not recognise one". Both shapes a client may use are counted here: a flat
// function named for plumb, and plumb tools nested under a plumb namespace. A
// non-plumb tool and a definition-less entry (like tool_search) are not.
func TestMeasureStepCountsDirectPlumbShapes(t *testing.T) {
	var tools []any
	if err := json.Unmarshal([]byte(`[
		{"type":"function","name":"mcp__plumb__read_file","parameters":{"type":"object"}},
		{"type":"namespace","name":"mcp__plumb__","tools":[
			{"type":"function","name":"edit_file","parameters":{"type":"object"}},
			{"type":"function","name":"git","parameters":{"type":"object"}}
		]},
		{"type":"function","name":"exec_command","parameters":{"type":"object"}},
		{"type":"tool_search"}
	]`), &tools); err != nil {
		t.Fatal(err)
	}
	s := measureStep(0, tools, nil)
	if s.PlumbDirect != 3 || s.PlumbDirectBytes == 0 {
		t.Fatalf("measureStep counted %d plumb schemas (%d bytes) in tools[], want 3 (one flat, two namespaced)", s.PlumbDirect, s.PlumbDirectBytes)
	}
	if s.SearchedTools != 0 {
		t.Fatalf("no input was given, yet %d schemas were counted as re-sent", s.SearchedTools)
	}
}
