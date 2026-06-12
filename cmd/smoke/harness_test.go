//go:build integration

// Package smoke_test exercises the full plumb stack end-to-end over the MCP
// wire protocol. It spawns a real `plumb serve` subprocess, speaks newline-
// delimited JSON-RPC 2.0 over its stdin/stdout, and verifies the responses.
//
// This file is the shared harness used by every scenario in the package:
//   - smoke_test.go      — the happy-path end-to-end walk against a Go fixture.
//   - tierd_test.go      — the parity guard plus the behaviours that are unsafe
//     or non-deterministic to drive against a live agent
//     (strict mode, transaction rollback, git tiers).
//   - reconnect_test.go  — the resilient-proxy crash-recovery test.
//
// Prerequisites: gopls must be on PATH for the Go-fixture scenarios; they skip
// otherwise.
// Run: go test -tags=integration -timeout=6m ./cmd/smoke/
package smoke_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── timeouts ────────────────────────────────────────────────────────────────

const (
	// sessionStartTimeout allows for gopls cold-start (JIT compile + indexing).
	sessionStartTimeout = 120 * time.Second
	// toolTimeout is used for all subsequent tool calls once gopls is warm.
	toolTimeout = 20 * time.Second
)

// ─── prerequisites ────────────────────────────────────────────────────────────

func requireGopls(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not found on PATH — install with: go install golang.org/x/tools/gopls@latest")
	}
}

// buildPlumb compiles the plumb binary from the repo root and returns its path.
func buildPlumb(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "plumb")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/plumb/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build plumb: %v\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found)")
		}
		dir = parent
	}
}

// ─── fixture ─────────────────────────────────────────────────────────────────

// makeFixture creates a temporary Go project with a .plumb/ marker. gopls
// requires an actual module with a go.mod to provide diagnostics. It copies the
// repo's shared testdata/go-fixture (which the gopls integration test also
// uses). The path is resolved via filepath.EvalSymlinks so file URIs match what
// gopls reports after it resolves macOS symlinks internally
// (/var/folders/… → /private/var/folders/…).
func makeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}

	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}

	must(os.Mkdir(filepath.Join(dir, ".plumb"), 0o755))

	src := filepath.Join(repoRoot(t), "testdata", "go-fixture")
	for _, name := range []string{"go.mod", "main.go"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal("copy fixture:", err)
		}
		must(os.WriteFile(filepath.Join(dir, name), b, 0o644))
	}
	return dir
}

// mkTmpHome (the isolated HOME under /tmp, kept short for macOS's 104-byte
// Unix-socket limit) lives in tierd_test.go, shared across the package.

// ─── MCP client ──────────────────────────────────────────────────────────────

// mcpMsg is the minimal JSON-RPC 2.0 envelope used for both requests and
// responses. ID is json.RawMessage to handle both integer and string forms.
type mcpMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// mcpClient wraps a plumb serve subprocess and provides a synchronous
// request/response interface over newline-delimited JSON-RPC 2.0.
//
// Concurrency: Send and Recv are safe for concurrent use.
type mcpClient struct {
	enc    *json.Encoder
	encMu  sync.Mutex
	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan mcpMsg // key: string(id JSON)

	// rootsPath is returned when the server sends a roots/list request.
	rootsPath string

	cancel context.CancelFunc
}

// newMCPClient starts a plumb serve subprocess and returns a ready client.
// extraEnv entries (e.g. "PLUMB_STRICT_EDITS=true") are appended to the
// isolated environment, letting a test pin a per-session config knob.
func newMCPClient(t *testing.T, ctx context.Context, plumbBin, tmpHome, rootsPath string, extraEnv ...string) *mcpClient {
	t.Helper()

	env := append(isolatedEnv(tmpHome), extraEnv...)

	cmd := exec.CommandContext(ctx, plumbBin, "serve")
	cmd.Env = env
	cmd.Stderr = os.Stderr

	stdinW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdoutR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal("start plumb serve:", err)
	}

	childCtx, cancel := context.WithCancel(ctx)
	c := &mcpClient{
		enc:       json.NewEncoder(stdinW),
		pending:   make(map[string]chan mcpMsg),
		rootsPath: rootsPath,
		cancel:    cancel,
	}
	c.enc.SetEscapeHTML(false)

	// Background reader: dispatch responses to pending waiters; answer
	// server-initiated requests (roots/list) inline.
	go c.readLoop(childCtx, bufio.NewReader(stdoutR))

	t.Cleanup(func() {
		cancel()
		stdinW.Close()
		cmd.Wait() //nolint:errcheck
	})

	// Stop the daemon we spawned at cleanup so it doesn't linger.
	t.Cleanup(func() {
		stopCmd := exec.Command(plumbBin, "stop", "--force")
		stopCmd.Env = env
		stopCmd.Run() //nolint:errcheck
	})

	return c
}

// isolatedEnv returns an environment slice that overrides HOME and every XDG
// dir to a temp directory, so the daemon plumb serve spawns uses a fresh,
// isolated socket / cache / config — leaving the developer's running daemon and
// global config untouched.
func isolatedEnv(tmpHome string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+8)
	for _, e := range base {
		switch {
		case strings.HasPrefix(e, "HOME="),
			strings.HasPrefix(e, "XDG_CONFIG_HOME="),
			strings.HasPrefix(e, "XDG_CACHE_HOME="),
			strings.HasPrefix(e, "XDG_DATA_HOME="),
			strings.HasPrefix(e, "XDG_STATE_HOME="):
			continue
		default:
			out = append(out, e)
		}
	}
	out = append(out,
		"HOME="+tmpHome,
		"XDG_CONFIG_HOME="+filepath.Join(tmpHome, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(tmpHome, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(tmpHome, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(tmpHome, ".local", "state"),
		// A cold gopls in a fresh workspace needs more than the 300 ms default
		// to emit the first diagnostics after a write.
		"PLUMB_POST_WRITE_DIAG_MS=5000",
	)
	return out
}

// readLoop reads all messages from the subprocess stdout and routes them.
func (c *mcpClient) readLoop(ctx context.Context, r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var msg mcpMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		// Server-initiated request (has both Method and ID set).
		if msg.Method != "" && len(msg.ID) > 0 {
			go c.handleServerRequest(ctx, msg)
			continue
		}

		// Response to one of our requests (has ID, no Method).
		if len(msg.ID) > 0 {
			c.pendingMu.Lock()
			ch, ok := c.pending[string(msg.ID)]
			c.pendingMu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}
		}
	}
}

// handleServerRequest answers server-initiated JSON-RPC requests.
// Currently handles roots/list; all others receive an empty-object result.
func (c *mcpClient) handleServerRequest(ctx context.Context, req mcpMsg) {
	var result any
	switch req.Method {
	case "roots/list":
		type root struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		}
		result = map[string]any{
			"roots": []root{{URI: "file://" + c.rootsPath, Name: "fixture"}},
		}
	default:
		result = map[string]any{}
	}
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(req.ID),
		"result":  result,
	}
	c.encMu.Lock()
	c.enc.Encode(resp) //nolint:errcheck
	c.encMu.Unlock()
}

// send writes a JSON-RPC request. Returns the ID used.
func (c *mcpClient) send(method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	rawID := json.RawMessage(fmt.Sprintf("%d", id))

	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(rawID),
		"method":  method,
	}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		msg["params"] = json.RawMessage(b)
	}
	// Register the pending response channel BEFORE writing the request, closing
	// the send→recv race: a daemon that answers faster than the caller reaches
	// recv() would otherwise have its response dropped by readLoop (no pending
	// entry yet), hanging the call until timeout. Surfaced as an intermittent,
	// load-sensitive, tool-agnostic smoke-test "hang".
	c.pendingMu.Lock()
	if _, exists := c.pending[string(rawID)]; !exists {
		c.pending[string(rawID)] = make(chan mcpMsg, 1)
	}
	c.pendingMu.Unlock()
	c.encMu.Lock()
	err := c.enc.Encode(msg)
	c.encMu.Unlock()
	return rawID, err
}

// notify sends a JSON-RPC notification (no response expected).
func (c *mcpClient) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		b, _ := json.Marshal(params)
		msg["params"] = json.RawMessage(b)
	}
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return c.enc.Encode(msg)
}

// recv waits for the response matching the given id within deadline. The
// pending channel is normally registered by send() before the request was
// written (closing the send→recv drop race); recv reuses it, registering one
// only if a caller used recv without a prior send().
func (c *mcpClient) recv(id json.RawMessage, timeout time.Duration) (mcpMsg, error) {
	key := string(id)
	c.pendingMu.Lock()
	ch := c.pending[key]
	if ch == nil {
		ch = make(chan mcpMsg, 1)
		c.pending[key] = ch
	}
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, key)
		c.pendingMu.Unlock()
	}()

	select {
	case msg := <-ch:
		return msg, nil
	case <-time.After(timeout):
		return mcpMsg{}, fmt.Errorf("timeout after %v waiting for response to id=%s", timeout, key)
	}
}

// call sends a tools/call request and returns the text content of the response.
// It fails the test on MCP-level errors, isError results, or timeout.
func (c *mcpClient) call(t *testing.T, toolName string, args map[string]any, timeout time.Duration) string {
	t.Helper()
	id, err := c.send("tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		t.Fatalf("tools/call %s: send: %v", toolName, err)
	}
	msg, err := c.recv(id, timeout)
	if err != nil {
		t.Fatalf("tools/call %s: %v", toolName, err)
	}
	if msg.Error != nil {
		t.Fatalf("tools/call %s: RPC error %d: %s", toolName, msg.Error.Code, msg.Error.Message)
	}
	text, isErr, derr := decodeToolResult(msg.Result)
	if derr != nil {
		t.Fatalf("tools/call %s: unmarshal result: %v\nraw: %s", toolName, derr, msg.Result)
	}
	if isErr {
		t.Fatalf("tools/call %s returned isError=true:\n%s", toolName, text)
	}
	return text
}

// initialize performs the MCP initialize / notifications/initialized handshake.
func (c *mcpClient) initialize(t *testing.T, rootsPath string) {
	t.Helper()
	id, err := c.send("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"roots": map[string]any{"listChanged": true},
		},
		"clientInfo": map[string]any{"name": "smoke-test", "version": "0.0.1"},
	})
	if err != nil {
		t.Fatal("initialize send:", err)
	}
	msg, err := c.recv(id, 15*time.Second)
	if err != nil {
		t.Fatal("initialize recv:", err)
	}
	if msg.Error != nil {
		t.Fatalf("initialize error: %s", msg.Error.Message)
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		t.Fatal("notifications/initialized:", err)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// decodeToolResult pulls the concatenated text content and isError flag from a
// tools/call result.
func decodeToolResult(raw json.RawMessage) (string, bool, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false, err
	}
	var texts []string
	for _, item := range result.Content {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	return strings.Join(texts, ""), result.IsError, nil
}

// assertContains fails the test if text does not contain want.
func assertContains(t *testing.T, label, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s: expected to contain %q\nfull output:\n%s", label, want, text)
	}
}

// extractMtime pulls the mtime value from a read_file header line.
// Header format: "# plumb-read mtime=<value> ..."
func extractMtime(t *testing.T, readOut string) string {
	t.Helper()
	for _, line := range strings.SplitN(readOut, "\n", 5) {
		if strings.HasPrefix(line, "# plumb-read") {
			for _, field := range strings.Fields(line) {
				if strings.HasPrefix(field, "mtime=") {
					return strings.TrimPrefix(field, "mtime=")
				}
			}
		}
	}
	t.Fatal("extractMtime: no mtime found in read_file output:\n" + readOut)
	return ""
}
