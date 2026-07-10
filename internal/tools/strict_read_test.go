package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Strict mode must be checked under the target's path lock, not in the unlocked
// preflight: a concurrent writer landing between an unlocked check and the write
// would leave strict mode passing on a file the session no longer knows. These
// two tests pin the split — the preflight must NOT enforce strict, and the gate
// (which applySingleEdit calls after lockPath) must.

func strictTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSemanticWritePreflight_DoesNotEnforceStrict(t *testing.T) {
	path := strictTestFile(t)
	deps := &WriteDeps{Reads: NewReadTracker(), Strict: func() bool { return true }}
	// No read recorded: if the preflight still enforced strict, this would fail.
	if err := semanticWritePreflight(context.Background(), deps, "replace_symbol_body", path, false, true); err != nil {
		t.Fatalf("strict must be gated under the path lock, not in the preflight: %v", err)
	}
}

func TestSemanticStrictGate_EnforcesReadBeforeWrite(t *testing.T) {
	path := strictTestFile(t)
	reads := NewReadTracker()
	deps := &WriteDeps{Reads: reads, Strict: func() bool { return true }}

	err := semanticStrictGate(deps, "replace_symbol_body", path)
	if err == nil || !strings.Contains(err.Error(), "has not been read in this daemon session") {
		t.Fatalf("an unread file must be refused under strict mode, got: %v", err)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	reads.Record(path, info.ModTime(), "")
	if err := semanticStrictGate(deps, "replace_symbol_body", path); err != nil {
		t.Fatalf("a read file must pass the gate: %v", err)
	}

	// A write that landed after the recorded read — the case the gate exists to
	// catch, and the reason it must run while the path lock is held.
	if err := os.WriteFile(path, []byte("package p // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if err := semanticStrictGate(deps, "replace_symbol_body", path); err == nil ||
		!strings.Contains(err.Error(), "has changed since you read it") {
		t.Fatalf("a file changed since the recorded read must be refused, got: %v", err)
	}
}

// The two tests above pin WHICH function enforces strict mode. This one pins the
// ordering that makes the enforcement sound: the gate must run while
// applySingleEdit holds the target's path lock. Asserting the lock is held at the
// time `resolve` runs would NOT discriminate — the lock is held there under both
// the correct order (lock → gate → resolve) and the bug this replaced (gate →
// lock → resolve). So probe from inside the gate itself: deps.Strict is called by
// semanticStrictGate, so a second goroutine's lockPath must block for as long as
// we sit in that callback.
func TestSemanticStrictGate_RunsUnderPathLock(t *testing.T) {
	path := strictTestFile(t)
	reads := NewReadTracker()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reads.Record(path, info.ModTime(), "")

	var (
		wg         sync.WaitGroup
		lockedHere bool
		gateRan    bool
	)

	deps := &WriteDeps{Reads: reads, Strict: func() bool {
		gateRan = true
		acquired := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockPath(path)
			close(acquired)
			unlock()
		}()
		select {
		case <-acquired:
			lockedHere = false // the lock was free — the gate is running unlocked
		case <-time.After(100 * time.Millisecond):
			lockedHere = true // still blocked — applySingleEdit holds it
		}
		return true
	}}

	sentinel := errors.New("resolve must not be reached")
	resolve := func(context.Context) (protocol.TextEdit, *protocol.DocumentSymbol, string, error) {
		return protocol.TextEdit{}, nil, "", sentinel
	}

	// The recorded read matches the file's mtime, so the gate passes and the call
	// proceeds to resolve, which refuses — no write is attempted.
	_, err = applySingleEdit(context.Background(), nil, nil, deps, path,
		false /*dryRun*/, false /*showDiff*/, "replace", "replace_symbol_body", true /*dirtyOK*/, resolve)
	wg.Wait() // the probe goroutine acquires once applySingleEdit releases

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the resolver's error to surface, got: %v", err)
	}
	if !gateRan {
		t.Fatal("the strict gate never ran — the test asserts nothing")
	}
	if !lockedHere {
		t.Error("the strict gate ran without the path lock held: a concurrent writer could land " +
			"between the mtime check and the write, and strict mode would pass on a file the " +
			"session no longer knows")
	}
}

// Strict mode off, or no deps at all, must leave the gate a no-op.
func TestSemanticStrictGate_NoOpWhenDisabled(t *testing.T) {
	path := strictTestFile(t)
	if err := semanticStrictGate(nil, "replace_symbol_body", path); err != nil {
		t.Errorf("nil deps must be a no-op: %v", err)
	}
	deps := &WriteDeps{Reads: NewReadTracker(), Strict: func() bool { return false }}
	if err := semanticStrictGate(deps, "replace_symbol_body", path); err != nil {
		t.Errorf("strict off must be a no-op: %v", err)
	}
}
