package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func ompStatesByEvent(t *testing.T, path, plumbBin string) map[string]string {
	t.Helper()
	entries := append(ompHookEntries(plumbBin), ompContextHookEntries(plumbBin)...)
	states, err := ompHooksTarget.statesAt(path, entries, plumbBin)
	if err != nil {
		t.Fatalf("statesAt: %v", err)
	}
	out := map[string]string{}
	for _, s := range states {
		out[s.entry.event] = s.state
	}
	return out
}

// TestOmpHookSource_Lifecycle walks the extension file through every state the
// status table and the writers distinguish: absent, installed, stale after the
// binary moves, narrowed by `uninstall --only context`, and removed.
func TestOmpHookSource_Lifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "extensions", "plumb.ts")
	all := append(ompHookEntries(""), ompContextHookEntries("")...)

	for ev, st := range ompStatesByEvent(t, path, "/opt/plumb") {
		if st != hookStateMissing {
			t.Errorf("absent file: %s = %s, want missing", ev, st)
		}
	}

	if changed, err := ompHooksTarget.installAt(path, all, "/opt/plumb"); err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	for ev, st := range ompStatesByEvent(t, path, "/opt/plumb") {
		if st != hookStateInstalled {
			t.Errorf("after install: %s = %s, want installed", ev, st)
		}
	}
	if changed, err := ompHooksTarget.installAt(path, all, "/opt/plumb"); err != nil || changed {
		t.Errorf("re-install must be a no-op: changed=%v err=%v", changed, err)
	}

	for ev, st := range ompStatesByEvent(t, path, "/moved/plumb") {
		if st != hookStateStale {
			t.Errorf("after the binary moved: %s = %s, want stale", ev, st)
		}
	}

	scope, _ := hooksUninstallScope("context")
	removed, err := ompHooksTarget.removeAt(path, scope, "/opt/plumb")
	if err != nil || removed != 2 {
		t.Fatalf("uninstall --only context: removed=%d err=%v, want 2", removed, err)
	}
	got := ompStatesByEvent(t, path, "/opt/plumb")
	if got["SessionStart"] != hookStateInstalled || got["Stop"] != hookStateInstalled {
		t.Errorf("linkage and mailbox must survive --only context: %v", got)
	}
	if got["UserPromptSubmit"] != hookStateMissing || got["SubagentStart"] != hookStateMissing {
		t.Errorf("context hints must be gone after --only context: %v", got)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "const HINTS = false;") || !strings.Contains(string(data), "const LINK = true;") {
		t.Errorf("narrowed file must switch hints off and keep linkage on:\n%s", data)
	}
	if baks, _ := filepath.Glob(path + ".*.bak"); len(baks) != 0 {
		t.Errorf("rewriting plumb's own untouched file must not back it up, got %v", baks)
	}

	// An edited copy is saved before plumb replaces it.
	if err := os.WriteFile(path, append(data, []byte("// my note\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ompHooksTarget.installAt(path, ompHookEntries(""), "/opt/plumb"); err != nil {
		t.Fatalf("install over an edited file: %v", err)
	}
	if baks, _ := filepath.Glob(path + ".*.bak"); len(baks) != 1 {
		t.Errorf("an edited file must be backed up once before replacement, got %v", baks)
	}

	removed, err = ompHooksTarget.removeAt(path, uninstallScope{}, "/opt/plumb")
	if err != nil || removed != 2 {
		t.Fatalf("full uninstall: removed=%d err=%v, want 2", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file left with no hooks must be deleted, stat err=%v", err)
	}
}

// TestOmpHookSource_RefusesAFileItDidNotWrite pins ownership: a plumb.ts the
// user wrote is never overwritten, reported as plumb's, or deleted.
func TestOmpHookSource_RefusesAFileItDidNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plumb.ts")
	const mine = "export default function (pi) {}\n"
	if err := os.WriteFile(path, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	all := append(ompHookEntries(""), ompContextHookEntries("")...)

	if _, err := ompHooksTarget.statesAt(path, all, "/opt/plumb"); err == nil {
		t.Error("status must report a foreign file as an error, not as hooks")
	}
	if _, err := ompHooksTarget.installAt(path, all, "/opt/plumb"); err == nil {
		t.Error("install must refuse to overwrite a foreign file")
	}
	if _, err := ompHooksTarget.removeAt(path, uninstallScope{}, "/opt/plumb"); err == nil {
		t.Error("uninstall must refuse to delete a foreign file")
	}
	if data, _ := os.ReadFile(path); string(data) != mine {
		t.Errorf("foreign file changed: %q", data)
	}
}

func TestOmpHookResult(t *testing.T) {
	mail := func(n int) func(string, string) (mailReport, bool) {
		return func(string, string) (mailReport, bool) { return mailReport{Count: n}, true }
	}
	noHint := func(contextHintRequest) string { return "" }

	t.Run("first prompt states the linkage", func(t *testing.T) {
		out := ompHookResult(ompHookInput{Event: "UserPromptSubmit", SessionID: "s-1", Link: true}, nil, noHint)
		if out == nil || !strings.Contains(out.Context, `session_start({session_id: "s-1"})`) {
			t.Fatalf("linkage missing: %+v", out)
		}
	})
	t.Run("a later prompt with nothing to say is silent", func(t *testing.T) {
		if out := ompHookResult(ompHookInput{Event: "UserPromptSubmit", SessionID: "s-1", Hint: true}, nil, noHint); out != nil {
			t.Errorf("want no output, got %+v", out)
		}
	})
	t.Run("no session id is silent", func(t *testing.T) {
		if out := ompHookResult(ompHookInput{Event: "UserPromptSubmit", Link: true}, nil, noHint); out != nil {
			t.Errorf("want no output, got %+v", out)
		}
	})
	t.Run("unread mail blocks the stop", func(t *testing.T) {
		out := ompHookResult(ompHookInput{Event: "Stop", SessionID: "s-1"}, mail(3), noHint)
		if out == nil || out.Decision != "block" || !strings.Contains(out.Reason, "3 unread") {
			t.Fatalf("want a block naming 3 unread, got %+v", out)
		}
	})
	t.Run("a stop already continued by a hook passes", func(t *testing.T) {
		if out := ompHookResult(ompHookInput{Event: "Stop", SessionID: "s-1", StopHookActive: true}, mail(3), noHint); out != nil {
			t.Errorf("want no output, got %+v", out)
		}
	})
	t.Run("an empty mailbox passes", func(t *testing.T) {
		if out := ompHookResult(ompHookInput{Event: "Stop", SessionID: "s-1"}, mail(0), noHint); out != nil {
			t.Errorf("want no output, got %+v", out)
		}
	})
}

// TestOmpExtensionSource_RecordsOnlyItsEvents pins the marker lines status and
// partial uninstall read back: a subset renders exactly that subset.
func TestOmpExtensionSource_RecordsOnlyItsEvents(t *testing.T) {
	src := ompExtensionSource(`/path with "quotes"/plumb`, []string{"Stop", "SessionStart"})
	if got := sourceEvents(src); !slices.Equal(got, []string{"SessionStart", "Stop"}) {
		t.Errorf("recorded events = %v, want [SessionStart Stop]", got)
	}
	if !strings.Contains(src, `const PLUMB: string = "/path with \"quotes\"/plumb";`) {
		t.Errorf("binary path must be a quoted string literal:\n%s", src)
	}
}
