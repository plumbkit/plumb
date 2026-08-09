//go:build clients

package clientsmoke

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// connectTimeout bounds a single client's connecting probe. A probe that hangs
// (a known cursor-agent failure mode) is killed at this deadline; the assertion
// then checks whether plumb saw the connection before the hang.
const connectTimeout = 45 * time.Second

// TestClientsConnect is the auth-free CONNECTION tier. For each client with an
// auth-free connecting probe, it confirms the CLI completes the MCP initialize
// handshake with plumb — asserted on plumb's own session file, independent of
// the CLI's output format. No API keys; deterministic. Clients without such a
// probe (codex/crush/goose/auggie) and uninstalled clients are skipped.
func TestClientsConnect(t *testing.T) {
	for _, spec := range clientSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			if !spec.connect {
				t.Skipf("no auth-free connection probe: %s", spec.connectSkip)
			}
			if _, err := exec.LookPath(spec.binary); err != nil {
				t.Skipf("%s not installed (%q not on PATH) — run scripts/install-clients.sh", spec.name, spec.binary)
			}

			tmpHome := mkTmpHome(t)
			fixture := makeBareFixture(t)
			env := isolatedEnv(tmpHome)
			t.Cleanup(func() { stopDaemon(tmpHome) })

			runPlumbSetup(t, env, spec.setupArgs...)
			if spec.prep != nil {
				spec.prep(t, tmpHome, fixture, env)
			}

			ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, spec.binary, spec.connectArgs...)
			cmd.Env = env
			if spec.probeEnv != nil {
				cmd.Env = append(cmd.Env, spec.probeEnv(realHome)...)
			}
			cmd.Dir = fixture
			out, runErr := cmd.CombinedOutput()
			t.Logf("$ %s %s  (exit=%v)\n%s", spec.binary, strings.Join(spec.connectArgs, " "), runErr, truncate(out, 2000))

			sess, ok := findClientSession(t, tmpHome)
			if !ok {
				t.Fatalf("FAIL %s: plumb recorded no client session — %q did not complete an MCP handshake with plumb",
					spec.name, spec.binary)
			}
			t.Logf("PASS %s: plumb saw client_name=%q version=%q (session %s)",
				spec.name, sess.ClientName, sess.ClientVersion, sess.ID)

			for _, w := range spec.wantOut {
				if !strings.Contains(string(out), w) {
					t.Logf("note: probe output did not contain %q (primary session signal still passed)", w)
				}
			}
		})
	}
}

// TestRawMCPStatsEvidence is the free, deterministic regression for the auth
// tier's success signal: a real MCP tools/call must become visible in stats while
// the isolated daemon is still running. It needs no client CLI or provider key.
func TestRawMCPStatsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stripXDG bool
	}{
		{name: "explicit-xdg"},
		{name: "client-filtered-xdg", stripXDG: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runRawMCPStatsEvidence(t, tc.stripXDG)
		})
	}
}

func runRawMCPStatsEvidence(t *testing.T, stripXDG bool) {
	t.Helper()
	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	env := isolatedEnv(tmpHome)
	if stripXDG {
		filtered := make([]string, 0, len(env))
		for _, e := range env {
			if !strings.HasPrefix(e, "XDG_") {
				filtered = append(filtered, e)
			}
		}
		env = filtered
	}
	t.Cleanup(func() { stopDaemon(tmpHome) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, plumbBin, "serve")
	cmd.Env = env
	cmd.Dir = fixture
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("raw MCP stdin:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("raw MCP stdout:", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal("start raw MCP proxy:", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	})

	enc := json.NewEncoder(stdin)
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "clientsmoke-raw", "version": "1"},
		},
	}); err != nil {
		t.Fatal("send initialize:", err)
	}
	readRawResponse(t, scan, "1", &stderr)
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}}); err != nil {
		t.Fatal("send initialized:", err)
	}
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "find_files", "arguments": map[string]any{"path": fixture}},
	}); err != nil {
		t.Fatal("send find_files:", err)
	}
	readRawResponse(t, scan, "2", &stderr)

	n, tools := pollToolCalls(t, tmpHome, 8*time.Second)
	if n == 0 {
		t.Fatalf("raw MCP tools/call succeeded but stats stayed empty; stderr:\n%s", stderr.String())
	}
	if !strings.Contains(tools, "find_files") {
		t.Fatalf("stats tools = %q, want find_files", tools)
	}
	t.Logf("raw MCP evidence: %d tool call(s) [%s] recorded before daemon teardown", n, tools)
}

func TestRawMCPDeterministicScenario(t *testing.T) {
	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	readPath := filepath.Join(fixture, "read.txt")
	editPath := filepath.Join(fixture, "edit.txt")
	if err := os.WriteFile(readPath, []byte("clientsmoke-read-ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(editPath, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, ".plumb", "config.toml"), []byte("[edits]\nstrict = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := conformanceEnv(tmpHome, "PLUMB_STRICT_EDITS=1")
	t.Cleanup(func() { stopDaemon(tmpHome) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, plumbBin, "serve")
	cmd.Env = env
	cmd.Dir = fixture
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("raw MCP stdin:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("raw MCP stdout:", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal("start raw MCP proxy:", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Wait()
	})

	enc := json.NewEncoder(stdin)
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	if err := enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "clientsmoke-raw", "version": "1"},
		},
	}); err != nil {
		t.Fatal("send initialize:", err)
	}
	readRawResponse(t, scan, "1", &stderr)
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}}); err != nil {
		t.Fatal("send initialized:", err)
	}

	nextID := 2
	request := func(method string, params map[string]any, allowToolError bool) rawMCPResponse {
		t.Helper()
		id := strconv.Itoa(nextID)
		nextID++
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nextID - 1, "method": method, "params": params}); err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
		return readRawResponseWithToolError(t, scan, id, &stderr, allowToolError)
	}
	call := func(stageIndex int, arguments map[string]any, wantError bool) string {
		t.Helper()
		stage := deterministicConformanceScenario[stageIndex]
		response := request("tools/call", map[string]any{"name": stage.tool, "arguments": arguments}, wantError)
		result := decodeRawToolResult(t, response)
		if result.IsError != wantError {
			t.Fatalf("%s: isError=%v, want %v: %s", stage.name, result.IsError, wantError, result.text())
		}
		return result.text()
	}

	listResponse := request(deterministicConformanceScenario[stageDiscovery].tool, map[string]any{}, false)
	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResponse.Result, &listed); err != nil {
		t.Fatal("decode tools/list:", err)
	}
	listedNames := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		listedNames[tool.Name] = true
	}
	for _, required := range conformanceRequiredTools() {
		if !listedNames[required] {
			t.Fatalf("discovery: tools/list omitted %s", required)
		}
	}

	call(stageSessionStart, map[string]any{"purpose": "raw-client-conformance"}, false)
	if body := call(stageReadSuccess, map[string]any{"file_path": readPath}, false); !strings.Contains(body, "clientsmoke-read-ok") {
		t.Fatalf("path_read: missing fixture content: %s", body)
	}
	if body := call(stageEditRefusal, map[string]any{
		"file_path": editPath,
		"edits":     []map[string]string{{"old_string": "before", "new_string": "after"}},
	}, true); !strings.Contains(body, "has not been read") {
		t.Fatalf("known_refusal: missing strict-read remediation: %s", body)
	}
	readBody := call(stageReadRemediation, map[string]any{"file_path": editPath}, false)
	mtime := extractHeaderToken(readBody, "mtime=")
	if mtime == "" {
		t.Fatal("advertised_remediation: read returned no mtime")
	}
	call(stageEditRemediated, map[string]any{
		"file_path": editPath, "expected_mtime": mtime,
		"edits": []map[string]string{{"old_string": "before", "new_string": "after"}},
	}, false)
	readBody = call(stageReadBeforeReconnect, map[string]any{"file_path": editPath}, false)
	mtime = extractHeaderToken(readBody, "mtime=")
	if mtime == "" || !strings.Contains(readBody, "after") {
		t.Fatalf("pre_reconnect_read: missing edited content or mtime: %s", readBody)
	}
	stopDaemon(tmpHome)
	call(stageEditAfterReconnect, map[string]any{
		"file_path": editPath, "expected_mtime": mtime,
		"edits": []map[string]string{{"old_string": "after", "new_string": "after-reconnect"}},
	}, false)

	edited, err := os.ReadFile(editPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(edited) != "after-reconnect\n" {
		t.Fatalf("edited fixture = %q, want %q", edited, "after-reconnect\\n")
	}
	n, tools := pollToolCallsAtLeast(t, tmpHome, 7, 8*time.Second)
	if n < 7 {
		t.Fatalf("missing_stats: got %d calls [%s], want at least 7", n, tools)
	}
	t.Logf("raw conformance: advertised_tools=%d calls=%d tools=%s", len(listed.Tools), n, tools)
}

type rawToolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func decodeRawToolResult(t *testing.T, response rawMCPResponse) rawToolResult {
	t.Helper()
	var result rawToolResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal("decode tools/call result:", err)
	}
	return result
}

func (r rawToolResult) text() string {
	var parts []string
	for _, content := range r.Content {
		if content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type rawMCPResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func readRawResponse(t *testing.T, scan *bufio.Scanner, wantID string, stderr *bytes.Buffer) rawMCPResponse {
	t.Helper()
	return readRawResponseWithToolError(t, scan, wantID, stderr, false)
}

func readRawResponseWithToolError(t *testing.T, scan *bufio.Scanner, wantID string, stderr *bytes.Buffer, allowToolError bool) rawMCPResponse {
	t.Helper()
	for scan.Scan() {
		var msg rawMCPResponse
		if json.Unmarshal(scan.Bytes(), &msg) != nil || string(msg.ID) != wantID {
			continue
		}
		if msg.Error != nil {
			t.Fatalf("raw MCP response %s: %s\nstderr:\n%s", wantID, msg.Error.Message, stderr.String())
		}
		var result struct {
			IsError bool `json:"isError"`
		}
		if len(msg.Result) > 0 && json.Unmarshal(msg.Result, &result) == nil && result.IsError && !allowToolError {
			t.Fatalf("raw MCP tools/call %s returned isError=true: %s", wantID, msg.Result)
		}
		return msg
	}
	t.Fatalf("raw MCP response %s not received: %v\nstderr:\n%s", wantID, scan.Err(), stderr.String())
	return rawMCPResponse{}
}
