package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
