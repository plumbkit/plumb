package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// pool_reconfigure_window_test.go — the PLAN-539 follow-ups: one resolution
// per reconfigure, a stale resolve never undoing a newer restart, and no wake
// for a config already superseded.

// waitSettled waits until e has no restart running.
func waitSettled(t *testing.T, p *workspacePool, e *poolEntry) {
	t.Helper()
	eventually(t, 5*time.Second, "the restart never finished", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !e.reconfiguring
	})
}

// TestReconfigureEntry_ResolvesOnce: the effective config and the project
// config's validity come from one resolution.
func TestReconfigureEntry_ResolvesOnce(t *testing.T) {
	p, _ := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	before := p.langResolves.Load()
	if p.reconfigureEntry(e) {
		t.Fatal("control: an unchanged config started a restart")
	}
	if n := p.langResolves.Load() - before; n != 1 {
		t.Fatalf("one reconfigure resolved the config %d times, want 1", n)
	}
}

// TestReconfigureEntry_AStaleResolveDoesNotUndoANewerRestart: a reconfigure
// that resolved its config just before a newer one was published, and reaches
// its decision after that newer restart finished, re-resolves instead of
// restarting the server back onto the old config.
func TestReconfigureEntry_AStaleResolveDoesNotUndoANewerRestart(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	fired := false // the publish below re-enters this hook; only the outer call acts
	p.reconfigureResolved = func(*poolEntry) {
		if fired {
			return
		}
		fired = true
		// Inside the stale reconfigure's window: publish a newer config and
		// let its restart finish.
		p.applyGlobalLSP(withGoArgs(cfg, "50"))
		waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"50"}) })
		waitSettled(t, p, e)
	}
	p.reconfigureEntry(e) // resolved the original config, args 30
	waitSettled(t, p, e)
	if _, c, _ := entryNow(p, e); !slices.Equal(c.Args, []string{"50"}) {
		t.Fatalf("the server ended on args %v; a stale resolve undid the newer restart", c.Args)
	}
}

// heldAndBusy acquires e with a session hold, and starts a call on it that
// keeps a restart waiting until done is called.
func heldAndBusy(t *testing.T, p *workspacePool) (e *poolEntry, root string, done func()) {
	t.Helper()
	root = markedRoot(t, "go.mod")
	e, err := p.acquireLang(context.Background(), root, "go", true)
	if err != nil {
		t.Fatal(err)
	}
	_, done, err = p.acquireCall(context.Background(), root, "go")
	if err != nil {
		t.Fatal(err)
	}
	return e, root, done
}

// TestRestartWithConfig_DoesNotWakeASupersededConfig: a round that ends with
// a newer config already requested leaves its server asleep; the round for the
// newer config wakes it.
func TestRestartWithConfig_DoesNotWakeASupersededConfig(t *testing.T) {
	logs := captureLogs(t)
	p, cfg := reconfigPool(t)
	e, _, done := heldAndBusy(t, p)
	p.applyGlobalLSP(withGoArgs(cfg, "60"))
	p.applyGlobalLSP(withGoArgs(cfg, "61"))
	done()
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"61"}) })
	waitSettled(t, p, e)
	if !strings.Contains(logs.String(), "its wake waits for the newer config") {
		t.Errorf("the superseded round woke its server; log:\n%s", logs.String())
	}
	if _, _, state := entryNow(p, e); !state.running() {
		t.Fatalf("the held server ended %s, want running on the newest config", state)
	}
}

// TestRestartWithConfig_ADeferredWakeIsNotLost: when the request that deferred
// the wake turns out to need no restart, the server is still woken.
func TestRestartWithConfig_ADeferredWakeIsNotLost(t *testing.T) {
	p, cfg := reconfigPool(t)
	e, root, done := heldAndBusy(t, p)
	p.applyGlobalLSP(withGoArgs(cfg, "70"))
	p.reconfigureRoot(root) // the same config again: marks the round to re-check
	done()
	waitReconfigured(t, p, e, func(c config.LSPConfig) bool { return slices.Equal(c.Args, []string{"70"}) })
	waitSettled(t, p, e)
	eventually(t, 5*time.Second, "a held server whose wake was deferred was left asleep", func() bool {
		_, _, state := entryNow(p, e)
		return state.running()
	})
}

// TestWakeHeld_LeavesAnEntryANewerRestartOwns: the deferred wake does not wake
// an entry a newer restart has already taken — that restart wakes it itself.
func TestWakeHeld_LeavesAnEntryANewerRestartOwns(t *testing.T) {
	p, _ := reconfigPool(t)
	e, err := p.acquireLang(context.Background(), markedRoot(t, "go.mod"), "go", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.hibernateWhenIdle(e); err != nil {
		t.Fatal(err)
	}
	e.reconfiguring = true // hibernateWhenIdle returned holding p.mu
	p.mu.Unlock()
	p.wakeHeld(e)
	if _, _, state := entryNow(p, e); state != poolHibernated {
		t.Fatalf("an entry a newer restart owns was woken (%s)", state)
	}
	p.mu.Lock()
	e.reconfiguring = false
	p.mu.Unlock()
	p.wakeHeld(e)
	eventually(t, 5*time.Second, "control: a held, hibernated entry with no restart running was not woken", func() bool {
		_, _, state := entryNow(p, e)
		return state.running()
	})
}
