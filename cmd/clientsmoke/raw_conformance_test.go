//go:build clients || clients_conformance

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

func TestRawMCPDeterministicScenario(t *testing.T) {
	tmpHome := mkTmpHome(t)
	fixture := makeBareFixture(t)
	editPath := filepath.Join(fixture, "edit.txt")
	if err := os.WriteFile(filepath.Join(fixture, "read.txt"), []byte("clientsmoke-read-ok\n"), 0o600); err != nil {
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

	scenario := newConformanceScenario(tmpHome, fixture)
	body := ""
	for {
		action, err := scenario.next(body)
		if err != nil {
			t.Fatalf("shared scenario: %v", err)
		}
		if action.complete {
			break
		}
		response := request("tools/call", map[string]any{
			"name":      action.stage.tool,
			"arguments": action.arguments,
		}, action.stage.expectRefusal)
		result := decodeRawToolResult(t, response)
		if result.IsError != action.stage.expectRefusal {
			t.Fatalf("%s: isError=%v, want %v: %s", action.stage.name, result.IsError, action.stage.expectRefusal, result.text())
		}
		body = result.text()
	}

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
