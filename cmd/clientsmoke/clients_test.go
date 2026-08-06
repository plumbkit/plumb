//go:build clients

package clientsmoke

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
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

type rawMCPResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func readRawResponse(t *testing.T, scan *bufio.Scanner, wantID string, stderr *bytes.Buffer) rawMCPResponse {
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
		if len(msg.Result) > 0 && json.Unmarshal(msg.Result, &result) == nil && result.IsError {
			t.Fatalf("raw MCP tools/call %s returned isError=true: %s", wantID, msg.Result)
		}
		return msg
	}
	t.Fatalf("raw MCP response %s not received: %v\nstderr:\n%s", wantID, scan.Err(), stderr.String())
	return rawMCPResponse{}
}
