//go:build integration

package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestIntegration_PoolHibernateWake exercises the full hibernate→wake cycle
// against a real language server: a server starts and completes its handshake,
// the janitor hibernates it (process stopped, PID gone, entry + warm cache
// kept), and a later acquire restarts it as a fresh process that re-readies its
// proxy. gopls stands in for any heavyweight server (jdtls) — the mechanism is
// language-agnostic and gopls is always present in CI with a sub-second start.
//
// Unlike the unit test (which uses a no-op `sleep` process that never speaks
// LSP), this proves wake actually re-runs Initialize/Initialized and republishes
// a live adapter.
func TestIntegration_PoolHibernateWake(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}

	dir := freshTempDir(t) // outside the repo tree, so no ancestral go.mod interferes
	mustWrite(t, filepath.Join(dir, "go.mod"), "module hibernatetest\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc Hello() string { return \"hi\" }\n\nfunc main() { _ = Hello() }\n")

	pool := &workspacePool{
		entries:   make(map[poolKey]*poolEntry),
		baseCtx:   context.Background(),
		cacheTTL:  time.Minute,
		idleGrace: time.Minute,
		langs: []langConfig{{name: "go", cfg: config.LSPConfig{
			Command:     "gopls",
			RootMarkers: []string{"go.mod"},
			Enabled:     true,
			IdleTimeout: config.Duration{Duration: 50 * time.Millisecond},
		}}},
	}
	defer pool.close()

	ctx := context.Background()
	e, err := pool.acquireLang(ctx, dir, "go", true)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waitEntryReady(t, e, 30*time.Second)
	pid1 := e.sup.PID()
	if pid1 <= 0 {
		t.Fatal("expected a running gopls PID after acquire")
	}

	// Idle past the 50ms timeout, then run the janitor's hibernation pass.
	time.Sleep(100 * time.Millisecond)
	pool.hibernateIdle()
	if e.state != poolHibernated {
		t.Fatalf("state = %v, want poolHibernated", e.state)
	}
	if pid := e.sup.PID(); pid != 0 {
		t.Fatalf("gopls process (pid %d) still running after hibernation", pid)
	}
	if e.proxy.get() != nil {
		t.Fatal("proxy still live after hibernation")
	}

	// Next acquire wakes it: a fresh process re-readies the same entry.
	woken, err := pool.acquireLang(ctx, dir, "go", true)
	if err != nil {
		t.Fatalf("wake acquire: %v", err)
	}
	if woken != e {
		t.Fatal("wake created a new entry instead of restarting the existing one")
	}
	waitEntryReady(t, e, 30*time.Second)
	pid2 := e.sup.PID()
	if pid2 <= 0 {
		t.Fatal("expected a running gopls PID after wake")
	}
	if pid2 == pid1 {
		t.Fatalf("expected a new process after wake, still pid %d", pid2)
	}
}

// waitEntryReady polls until the entry's proxy publishes a live adapter (the
// handshake completed) or the deadline passes.
func waitEntryReady(t *testing.T, e *poolEntry, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if e.proxy.get() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("language server did not become ready within deadline")
}
