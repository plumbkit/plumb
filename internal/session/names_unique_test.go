package session

// Tests for the name-uniqueness internals. These live in package session rather
// than session_test because they drive the unexported draw hook and helpers
// directly — the random-collision paths are otherwise unreachable.

import (
	"strings"
	"testing"
	"time"
)

// stubDraw pins GenerateName's output for the duration of a test. Draws are
// consumed in order and the last one repeats, so a single argument means "the
// draw is permanently unlucky". Without this the collision and suffix paths are
// unreachable: a real draw picks from a few thousand names.
func stubDraw(t *testing.T, names ...string) {
	t.Helper()
	orig := generateName
	i := 0
	generateName = func() string {
		n := names[min(i, len(names)-1)]
		i++
		return n
	}
	t.Cleanup(func() { generateName = orig })
}

// TestFreeName_RedrawsPastACollision is the common case: one unlucky draw, then
// a free one.
func TestFreeName_RedrawsPastACollision(t *testing.T) {
	stubDraw(t, "taken-name", "taken-name", "free-name")
	live := []Info{{ID: "peer", Name: "taken-name"}}

	if got := freeName(live, "self"); got != "free-name" {
		t.Fatalf("freeName = %q, want free-name", got)
	}
}

// TestFreeName_SuffixesWhenEveryDrawCollides pins the fallback. With the draw
// permanently unlucky the name must still come back free, and it must skip a
// suffix another session already holds rather than stopping at the first one.
func TestFreeName_SuffixesWhenEveryDrawCollides(t *testing.T) {
	stubDraw(t, "amber-antelope")
	live := []Info{
		{ID: "a", Name: "amber-antelope"},
		{ID: "b", Name: "amber-antelope-2"},
	}

	if got := freeName(live, "self"); got != "amber-antelope-3" {
		t.Fatalf("freeName = %q, want amber-antelope-3", got)
	}
}

// TestFreeName_IgnoresOwnName: selfID is excluded, or re-registering an ID would
// suffix a name that session legitimately already holds.
func TestFreeName_IgnoresOwnName(t *testing.T) {
	stubDraw(t, "mine")
	live := []Info{{ID: "self", Name: "mine"}}

	if got := freeName(live, "self"); got != "mine" {
		t.Fatalf("freeName = %q, want mine", got)
	}
}

// TestNameTaken_IsCaseInsensitive documents a deliberate mismatch with delivery:
// collab matches addressees with SQLite's case-sensitive '=', so being stricter
// here can only refuse confusable names, never admit an ambiguous address.
func TestNameTaken_IsCaseInsensitive(t *testing.T) {
	live := []Info{{ID: "peer", Name: "Reviewer"}}

	if !nameTaken(live, "reviewer", "self") {
		t.Error("nameTaken did not match across case")
	}
	if nameTaken(live, "reviewer", "peer") {
		t.Error("nameTaken matched the session's own name; selfID must be excluded")
	}
}

// TestWithSuffix_SurvivesNormaliseName: a suffixed name is stored and then
// re-validated on every restore, so it has to satisfy the same rules a
// user-supplied name does — including the length cap.
func TestWithSuffix_SurvivesNormaliseName(t *testing.T) {
	long := strings.Repeat("a", MaxNameLength)

	for _, n := range []int{2, 11, 250} {
		got := withSuffix(long, n)
		if len(got) > MaxNameLength {
			t.Errorf("withSuffix(_, %d) = %q (%d chars), over the %d cap", n, got, len(got), MaxNameLength)
		}
		if _, err := NormaliseName(got); err != nil {
			t.Errorf("withSuffix(_, %d) = %q, rejected by NormaliseName: %v", n, got, err)
		}
	}
}

// TestWithSuffix_NeverLeavesATrailingHyphen covers the trim landing exactly on a
// hyphen, which a naive cut would turn into a trailing hyphen — rejected by
// NormaliseName, so the name would be unrestorable after a daemon restart.
func TestWithSuffix_NeverLeavesATrailingHyphen(t *testing.T) {
	base := strings.Repeat("a", MaxNameLength-3) + "-bb" // hyphen sits at the trim point

	got := withSuffix(base, 7)
	if strings.HasSuffix(got, "-") || strings.Contains(got, "--") {
		t.Fatalf("withSuffix = %q, want no trailing or doubled hyphen", got)
	}
	if _, err := NormaliseName(got); err != nil {
		t.Fatalf("withSuffix = %q, rejected by NormaliseName: %v", got, err)
	}
}

// TestRegisterAndRename_DoNotSelfDeadlock guards the flock's non-reentrancy.
// The uniqueness checks run inside withSessionDirLock, so they must go through
// listLocked; swapping in List() would take the lock a second time and block
// forever. That failure hangs rather than returning a wrong value, so this
// asserts on a deadline instead of on a result.
func TestRegisterAndRename_DoNotSelfDeadlock(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	done := make(chan struct{})
	go func() {
		defer close(done)
		reg, err := Register(Info{})
		if err != nil {
			return
		}
		defer Unregister(reg.ID)
		_, _ = Rename(reg.ID, "renamed-ok")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Register/Rename did not complete: a nested List() inside the session-directory flock deadlocks")
	}
}
