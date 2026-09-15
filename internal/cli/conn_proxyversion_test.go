package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_proxyversion_test.go closes the daemon-side half of the proxy-version
// chain: that the hook is WIRED, and that what arrives on it reaches the row an
// agent actually reads.
//
// internal/mcp pins the other half (initialize `_meta` → OnProxyVersion firing).
// Between them every link is held: sent → read → stored → rendered. That
// matters more than usual here, because a break anywhere in the chain renders
// "unknown — no serve proxy declared one", which is indistinguishable from a
// legitimately-old proxy. A silent failure and a correct degradation produce the
// same string, so only a test that asserts the KNOWN-TRUE direction can tell
// them apart.

// TestRegisterHooks_WiresProxyVersionThroughToDaemonInfo drives the real
// registerHooks wiring, not a hand-set field: deleting the srv.OnProxyVersion
// assignment must fail something.
func TestRegisterHooks_WiresProxyVersionThroughToDaemonInfo(t *testing.T) {
	s := &connSession{}
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: "0.19.3"})
	s.registerHooks(srv)

	if srv.OnProxyVersion == nil {
		t.Fatal("registerHooks left OnProxyVersion unwired; a version the proxy " +
			"declares would be dropped and the row would read as an old proxy")
	}
	srv.OnProxyVersion(context.Background(), "0.19.2")

	if got := s.proxyVersion(); got != "0.19.2" {
		t.Fatalf("session proxy version = %q, want 0.19.2", got)
	}

	// And through to the rendered row, wired exactly as conn_register wires it.
	info := tools.NewDaemonInfo("sess-1", "swift-falcon", "0.19.3", time.Now()).
		WithProxyVersion(s.proxyVersion)
	out, err := info.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("daemon_info: %v", err)
	}
	if !strings.Contains(out, "0.19.2") || !strings.Contains(out, "DIFFERS") {
		t.Errorf("a declared proxy version must reach the row and be flagged as "+
			"differing from the daemon:\n%s", out)
	}
	// The failure mode this whole chain exists to avoid: reporting the absent
	// case when the version was in fact declared.
	if strings.Contains(out, "no serve proxy declared one") {
		t.Errorf("row reports an undeclared proxy although one was declared:\n%s", out)
	}
}

// TestDaemonInfo_UndeclaredProxyReadsUnknown is the known-false half, and it is
// only meaningful beside the test above. On its own it passes whether or not the
// chain works — which is exactly how the gap survived review.
func TestDaemonInfo_UndeclaredProxyReadsUnknown(t *testing.T) {
	s := &connSession{}
	info := tools.NewDaemonInfo("sess-1", "swift-falcon", "0.19.3", time.Now()).
		WithProxyVersion(s.proxyVersion)
	out, err := info.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("daemon_info: %v", err)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("a session no proxy declared a version for must read unknown:\n%s", out)
	}
	if strings.Contains(out, "DIFFERS") || strings.Contains(out, "matches this daemon") {
		t.Errorf("an undeclared proxy must not be reported as differing or matching:\n%s", out)
	}
}
