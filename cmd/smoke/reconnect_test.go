//go:build integration

package smoke_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSmoke_ProxyReconnect exercises the headline 0.8.0 feature: the resilient
// reconnecting `plumb serve` proxy. It establishes a live MCP session, kills
// the daemon out from under the proxy (simulating a crash), and asserts that a
// brand-new tool call still succeeds — proving the proxy dial-or-spawned a
// fresh daemon and transparently replayed the initialize handshake. That the
// recovery daemon is genuinely new is confirmed by the pid file changing.
//
// Only daemon-local tools (version, daemon_info) are used, so no gopls
// cold-start is involved and recovery is fast. This is a Tier-D behaviour
// (unsafe/non-deterministic to drive against a live agent), deferred here per
// internal/mcp/selftest_prompt.go.
func TestSmoke_ProxyReconnect(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeFixture(t) // version/daemon_info never attach the workspace, so gopls never starts
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	c.initialize(t, fixture)

	// Baseline: the proxy is up and the daemon answers.
	v1 := c.call(t, "version", map[string]any{}, toolTimeout)
	assertContains(t, "version(baseline)", v1, runtime.GOOS)

	pid1 := waitForPID(t, tmpHome, 10*time.Second)
	t.Logf("daemon up, pid=%s; killing it", pid1)

	// Kill the daemon. `plumb stop` SIGTERMs the pid in the isolated cache dir.
	stop := exec.Command(plumbBin, "stop", "--force")
	stop.Env = isolatedEnv(tmpHome)
	if out, err := stop.CombinedOutput(); err != nil {
		// Best-effort: the proxy's heartbeat may have already reaped the daemon;
		// what matters is the recovery assertion below.
		t.Logf("plumb stop: %v\n%s", err, out)
	}

	// A fresh request must succeed once the proxy has respawned the daemon and
	// replayed the handshake. The first attempt(s) may see the synthesised
	// retryable error (-32000) while reconnect is in flight, so we retry.
	var recovered string
	deadline := time.Now().Add(45 * time.Second)
	for {
		if txt, ok := c.callAllowError("version", map[string]any{}, toolTimeout); ok {
			recovered = txt
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy did not recover: version still failing after the daemon was killed")
		}
		time.Sleep(500 * time.Millisecond)
	}
	assertContains(t, "version(recovered)", recovered, runtime.GOOS)

	// daemon_info must also work against the respawned daemon.
	c.call(t, "daemon_info", map[string]any{}, toolTimeout)

	// Prove it is a NEW daemon: the pid must change.
	pid2 := waitForNewPID(t, tmpHome, pid1, 15*time.Second)
	if pid2 == "" {
		t.Fatal("no daemon pid file after recovery")
	}
	if pid1 != "" && pid1 == pid2 {
		t.Errorf("daemon pid unchanged after kill (%s); expected a respawned daemon", pid1)
	}
	t.Logf("proxy recovered transparently: daemon pid %s → %s", pid1, pid2)
}

// callAllowError is like mcpClient.call but never fails the test: it returns
// the text (or RPC error message) and whether the call succeeded. Used while
// the proxy is mid-reconnect, where transient failures are expected and retried.
func (c *mcpClient) callAllowError(toolName string, args map[string]any, timeout time.Duration) (string, bool) {
	id, err := c.send("tools/call", map[string]any{"name": toolName, "arguments": args})
	if err != nil {
		return "", false
	}
	msg, err := c.recv(id, timeout)
	if err != nil {
		return "", false
	}
	if msg.Error != nil {
		return msg.Error.Message, false
	}
	text, isErr, derr := decodeToolResult(msg.Result)
	if derr != nil {
		return "", false
	}
	return text, !isErr
}

// daemonPIDPath returns where the isolated daemon writes its pid file, matching
// os.UserCacheDir() semantics for the platform.
func daemonPIDPath(tmpHome string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(tmpHome, "Library", "Caches", "plumb", "plumb.pid")
	}
	return filepath.Join(tmpHome, ".cache", "plumb", "plumb.pid")
}

// readDaemonPID returns the current daemon pid, or "" if not present/readable.
func readDaemonPID(tmpHome string) string {
	b, err := os.ReadFile(daemonPIDPath(tmpHome))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// waitForPID polls until the daemon pid file is present and non-empty.
func waitForPID(t *testing.T, tmpHome string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := readDaemonPID(tmpHome); pid != "" {
			return pid
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("daemon pid file never appeared at %s", daemonPIDPath(tmpHome))
	return ""
}

// waitForNewPID polls until the daemon pid file differs from old.
func waitForNewPID(t *testing.T, tmpHome, old string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := readDaemonPID(tmpHome); pid != "" && pid != old {
			return pid
		}
		time.Sleep(200 * time.Millisecond)
	}
	return readDaemonPID(tmpHome)
}
