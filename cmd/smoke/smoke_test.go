//go:build integration

// Package smoke_test exercises the full plumb stack end-to-end over the MCP
// wire protocol. It spawns a real `plumb serve` subprocess, speaks newline-
// delimited JSON-RPC 2.0 over its stdin/stdout, and verifies the responses
// match the assertions in docs/internal/claude-desktop-smoke.md.
//
// Prerequisites: gopls must be on PATH.
// Run: go test -tags=integration -timeout=3m ./cmd/smoke/
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
// requires an actual module with a go.mod to provide diagnostics.
// The path is resolved via filepath.EvalSymlinks so that the URI we pass to
// plumb matches the real path gopls sees after it resolves symlinks internally
// (macOS: /var/folders/… is a symlink to /private/var/folders/…).
// makeFixture creates a temporary Go project for the smoke test by copying
// the repo's shared testdata/go-fixture (which the gopls integration test
// also uses) and adding a .plumb/ marker so plumb detects it as a workspace.
// The path is resolved via filepath.EvalSymlinks so that file URIs match
// what gopls returns after it resolves macOS symlinks internally
// (/var/folders/… → /private/var/folders/…).
func makeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Resolve symlinks so our file URIs match what gopls reports back.
	real, err := filepath.EvalSymlinks(dir)
	if err == nil {
		dir = real
	}

	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}

	must(os.Mkdir(filepath.Join(dir, ".plumb"), 0o755))

	// Copy go.mod and main.go from the shared testdata fixture.
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

// newMCPClient starts a plumb serve subprocess and performs the MCP
// initialise handshake. It returns a ready-to-use client.
func newMCPClient(t *testing.T, ctx context.Context, plumbBin, tmpHome, rootsPath string) *mcpClient {
	t.Helper()

	env := isolatedEnv(tmpHome)

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

// isolatedEnv returns an environment slice that overrides HOME (and XDG dirs
// on Linux) to a temp directory. This causes the plumb daemon that serve
// spawns to use a fresh, isolated socket path, leaving the developer's
// running daemon untouched.
func isolatedEnv(tmpHome string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+4)
	for _, e := range base {
		switch {
		case strings.HasPrefix(e, "HOME="),
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
		"XDG_CACHE_HOME="+filepath.Join(tmpHome, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(tmpHome, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(tmpHome, ".local", "state"),
		// Give a fresh gopls instance enough time to fully process the file and
		// emit diagnostics. The default 300 ms is tuned for a warm server; for
		// a cold start in a new workspace 5 s is safer.
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
			key := string(msg.ID)
			c.pendingMu.Lock()
			ch, ok := c.pending[key]
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

// recv waits for the response matching the given id within deadline.
func (c *mcpClient) recv(id json.RawMessage, timeout time.Duration) (mcpMsg, error) {
	key := string(id)
	ch := make(chan mcpMsg, 1)
	c.pendingMu.Lock()
	c.pending[key] = ch
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
// It fails the test on MCP-level errors or on timeout.
func (c *mcpClient) call(t *testing.T, toolName string, args map[string]any, timeout time.Duration) string {
	t.Helper()
	id, err := c.send("tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
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
	return extractText(t, toolName, msg.Result)
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

// extractText pulls the first text content item from a tools/call result.
func extractText(t *testing.T, toolName string, raw json.RawMessage) string {
	t.Helper()
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("tools/call %s: unmarshal result: %v\nraw: %s", toolName, err, raw)
	}
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	out := strings.Join(texts, "")
	if result.IsError {
		t.Fatalf("tools/call %s returned isError=true:\n%s", toolName, out)
	}
	return out
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

// ─── test ────────────────────────────────────────────────────────────────────

func TestSmoke_EndToEnd(t *testing.T) {
	requireGopls(t)

	plumbBin := buildPlumb(t)
	fixture := makeFixture(t)
	// Use /tmp (not t.TempDir) so the socket path stays under macOS's 104-byte
	// Unix socket path limit. t.TempDir() produces paths like
	//   /var/folders/bk/.../T/TestSmoke_EndToEnd…/001/Library/Caches/plumb/plumb.sock
	// which can exceed 104 bytes and cause net.Listen("unix", …) to fail.
	tmpHome, err := os.MkdirTemp("/tmp", "plsmk")
	if err != nil {
		t.Fatal("create tmpHome:", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpHome) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)

	// ── MCP handshake ────────────────────────────────────────────────────────
	client.initialize(t, fixture)

	// ── Step 1 + 2: session_start attaches workspace and returns orientation ─
	// The explicit workspace arg causes OnBeforeTool → attachWorkspace → gopls
	// to start. This is the slow step; we allow a generous timeout.
	t.Log("step 1+2: session_start (may wait for gopls to start…)")
	sessionOut := client.call(t, "session_start",
		map[string]any{"workspace": fixture},
		sessionStartTimeout,
	)
	assertContains(t, "session_start", sessionOut, "Language: Go")
	assertContains(t, "session_start", sessionOut, fixture)

	mainGo := filepath.Join(fixture, "main.go")

	// ── Step 3: read_file returns the mtime header ───────────────────────────
	t.Log("step 3: read_file")
	readOut := client.call(t, "read_file",
		map[string]any{"path": mainGo},
		toolTimeout,
	)
	assertContains(t, "read_file", readOut, "# plumb-read mtime=")
	assertContains(t, "read_file", readOut, "func main()")
	mtime := extractMtime(t, readOut)

	// ── Step 4: edit_file applies a valid change ─────────────────────────────
	t.Log("step 4: edit_file (valid change)")
	editOut := client.call(t, "edit_file", map[string]any{
		"path": mainGo,
		"edits": []map[string]any{
			{"old_str": `g.Greet("world")`, "new_str": `g.Greet("smoke test")`},
		},
		"expected_mtime": mtime,
		"dirty_ok":       true,
	}, toolTimeout)
	assertContains(t, "edit_file", editOut, "applied 1 edit")

	// ── Step 5: write_file creates a new broken file; gopls must report diagnostics ─
	// We write a brand-new file (FileCreated notification) so gopls loads it
	// fresh — the same pattern used in the gopls adapter integration test.
	// Editing an existing file (FileChanged) on a cold workspace can take longer
	// than the post-write diagnostics window because gopls may not have the file
	// in its in-memory view yet.
	t.Log("step 5: write_file (new file with syntax error — expect diagnostics)")
	brokenGo := filepath.Join(fixture, "broken.go")
	syntaxOut := client.call(t, "write_file", map[string]any{
		"path":    brokenGo,
		"content": "package main\n\nfunc broken( { } // missing closing paren\n",
	}, toolTimeout)
	assertContains(t, "write_file(syntax error)", syntaxOut, "diagnostics after write")

	// Remove broken.go so gopls is clean for any further steps.
	t.Log("step 5: removing broken.go")
	client.call(t, "delete_file", map[string]any{"path": brokenGo}, toolTimeout)

	// ── Step 7: list_memories returns without error ──────────────────────────
	t.Log("step 7: list_memories")
	memOut := client.call(t, "list_memories", map[string]any{}, toolTimeout)
	// An empty workspace has no memories; the response is a message, not an error.
	_ = memOut // any non-error response satisfies step 7
}
