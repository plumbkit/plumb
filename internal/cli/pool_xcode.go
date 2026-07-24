package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/xcodebsp"
)

type xcodeTrustChecker interface {
	IsTrusted(string) bool
}

type poolXcodeState struct {
	mu      sync.Mutex
	states  map[string]xcodebsp.Status
	running map[string]bool
	trust   xcodeTrustChecker
	runner  xcodebsp.Runner
	restart func(string) error // nil in production; deterministic coordinator test seam
}

func newPoolXcodeState() poolXcodeState {
	return poolXcodeState{
		states:  make(map[string]xcodebsp.Status),
		running: make(map[string]bool),
		trust:   config.NewTrustStore(),
		runner:  xcodeArgvRunner{},
	}
}

type xcodeArgvRunner struct{}

func (xcodeArgvRunner) Run(ctx context.Context, workdir string, argv []string, timeout time.Duration) (xcodebsp.ExecResult, error) {
	result, err := tools.RunArgv(ctx, workdir, argv, timeout)
	return xcodebsp.ExecResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		TimedOut: result.TimedOut,
	}, err
}

func canonicalXcodeRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}

// ensureXcodeBuildServer starts one background configuration flow per canonical
// root. Concurrent sessions observe the same state and never generate/restart twice.
func (p *workspacePool) ensureXcodeBuildServer(root string, cfg config.XcodeConfig) {
	if root == "" {
		return
	}
	root = canonicalXcodeRoot(root)
	if !xcodebsp.Inspect(root).IsBareXcode() {
		return
	}
	p.xcode.mu.Lock()
	if p.xcode.running == nil {
		p.xcode.running = make(map[string]bool)
	}
	if p.xcode.states == nil {
		p.xcode.states = make(map[string]xcodebsp.Status)
	}
	if p.xcode.running[root] {
		p.xcode.mu.Unlock()
		return
	}
	p.xcode.running[root] = true
	p.xcode.states[root] = xcodebsp.Status{State: xcodebsp.StateConfiguring}
	p.xcode.mu.Unlock()
	slog.Info("xcode: build-server lifecycle", "workspace", root, "state", xcodebsp.StateConfiguring)

	go func() {
		ctx := p.baseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		status := xcodebsp.Configure(ctx, xcodebsp.ConfigureRequest{
			Root: root, Scheme: cfg.Scheme, Timeout: cfg.Timeout.Duration,
			Enabled: cfg.AutoBuildServer, Trusted: p.xcode.trust != nil && p.xcode.trust.IsTrusted(root),
		}, p.xcode.runner)
		if status.State == xcodebsp.StateConfiguredNeedsBuildData {
			p.setXcodeStatus(root, status)
			p.setXcodeStatus(root, xcodebsp.Status{State: xcodebsp.StateRestarting, Detail: status.Detail, Selection: status.Selection})
			restart := p.restartSwift
			if p.xcode.restart != nil {
				restart = p.xcode.restart
			}
			if err := restart(root); err != nil {
				status = xcodebsp.Status{State: xcodebsp.StateFailed, Detail: err.Error(), Selection: status.Selection}
			} else {
				status.State = xcodebsp.StateWarming
				status.Detail = "SourceKit-LSP restarted with buildServer.json; build data may still be required"
			}
		}
		p.xcode.mu.Lock()
		p.xcode.states[root] = status
		delete(p.xcode.running, root)
		p.xcode.mu.Unlock()
		slog.Info("xcode: build-server lifecycle", "workspace", root, "state", status.State, "detail", status.Detail)
	}()
}

func (p *workspacePool) setXcodeStatus(root string, status xcodebsp.Status) {
	root = canonicalXcodeRoot(root)
	p.xcode.mu.Lock()
	if p.xcode.states == nil {
		p.xcode.states = make(map[string]xcodebsp.Status)
	}
	p.xcode.states[root] = status
	p.xcode.mu.Unlock()
	slog.Info("xcode: build-server lifecycle", "workspace", root, "state", status.State, "detail", status.Detail)
}

func (p *workspacePool) xcodeStatus(root string) xcodebsp.Status {
	root = canonicalXcodeRoot(root)
	p.xcode.mu.Lock()
	defer p.xcode.mu.Unlock()
	return p.xcode.states[root]
}

func (p *workspacePool) xcodeStatusJSON(root string) string {
	status := p.xcodeStatus(root)
	data, err := json.Marshal(status)
	if err != nil {
		return ""
	}
	return string(data) + "\n"
}

// markXcodeSemanticProven is called only after a non-empty SourceKit-LSP
// definition, reference, or workspace-symbol response.
func (p *workspacePool) markXcodeSemanticProven(root string) {
	root = canonicalXcodeRoot(root)
	if !xcodebsp.Inspect(root).BuildServerOK {
		return
	}
	p.setXcodeStatus(root, xcodebsp.Status{
		State:  xcodebsp.StateSemanticProven,
		Detail: "SourceKit-LSP returned a non-empty semantic result",
	})
}

// restartSwift reuses the pool's hibernate/wake lifecycle so refs, the proxy,
// supervisor hooks, and the watcher remain coherent for every attached session.
//
// Like awaitReady, this has a SLOW-failure hazard: wakeLocked sets the entry
// poolActive optimistically and returns before the woken Supervisor's first
// OnStart has actually completed. If that OnStart then fails AFTER we stop
// waiting (grace or ctx expiry, below), nobody is left reading readyCh unless
// we hand it to reapOnLateStartFailure — otherwise the entry is left
// poolActive with a dead proxy (proxy.get() == nil) and gets reused forever.
// removeFailed is the correct healing here too, not a narrower "mark for
// re-restart": wakeLocked reuses the SAME poolEntry/Supervisor rather than
// building a new one, and the Supervisor's loop already exited on the failed
// first start (no retry), so there is no live process left to keep — only
// eviction lets the NEXT acquire build a genuinely fresh entry. removeFailed is
// idempotent (map-identity guard + closeOnce), so it is safe even if a
// concurrent reapEntry/hibernate already touched this entry.
func (p *workspacePool) restartSwift(root string) error {
	e := p.lookup(root, "swift")
	if e == nil {
		return fmt.Errorf("restarting SourceKit-LSP: no Swift pool entry for %s", root)
	}
	p.hibernateEntry(e)

	p.mu.Lock()
	if e.state != poolHibernated {
		p.mu.Unlock()
		return fmt.Errorf("restarting SourceKit-LSP: entry did not hibernate")
	}
	if e.cache != nil {
		e.cache.Clear()
	}
	if e.inv != nil {
		e.inv.ClearPullState()
	}
	// An explicit restart is a genuine re-negotiation point (possibly a new server
	// build): drop any sticky -32601 downgrade so resolveDiagMode resolves from
	// config again, unlike a hibernation wake which preserves it.
	e.diagDowngraded = false
	ready, err := p.wakeLocked(e)
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("restarting SourceKit-LSP: %w", err)
	}
	ctx := p.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	grace := p.startGrace
	if grace <= 0 {
		grace = firstStartGrace
	}
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("restarting SourceKit-LSP: %w", err)
		}
		return nil
	case <-time.After(grace):
		go p.reapOnLateStartFailure(e, ready)
		return nil
	case <-ctx.Done():
		go p.reapOnLateStartFailure(e, ready)
		return ctx.Err()
	}
}
