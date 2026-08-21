package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// briefGitInit mirrors TestSessionStart_GitPolicySection's gitInit helper: an
// initialised-but-empty repo is enough for gitBranch(ws) to report a branch
// (git's unborn HEAD already names it), which is what gates both the full
// and brief git-policy sections on.
func briefGitInit(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if out, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	return ws
}

// writeBriefMemory drops a minimal user-authored memory file directly on
// disk — memory.List reads whatever is under .plumb/memories/*.md, so no
// write_memory tool plumbing is needed for a fixture.
func writeBriefMemory(t *testing.T, ws, name string) {
	t.Helper()
	dir := filepath.Join(ws, ".plumb", "memories")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := fmt.Sprintf("---\ndescription: a fairly long description that would bloat a full listing considerably\n---\n\n# %s\n\nsome body text\n", name)
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestSessionStartBrief_ByteBudget pins the card's literal acceptance bound:
// the brief render must stay at or under 1536 bytes even on a workspace with
// enough memories, diagnostics, and a live git policy to exercise every
// brief field — the caps (joinBriefNames, the diagnostics COUNT instead of
// bodies) are what keep it there regardless of workspace size.
func TestSessionStartBrief_ByteBudget(t *testing.T) {
	ws := briefGitInit(t)
	for i := range 15 {
		writeBriefMemory(t, ws, fmt.Sprintf("memory-%02d", i))
	}
	diags := map[string][]protocol.Diagnostic{}
	for i := range 20 {
		uri := fmt.Sprintf("file://%s/f%d.go", ws, i)
		diags[uri] = []protocol.Diagnostic{makeDiag(0, 0, "boom", protocol.SevError), makeDiag(1, 0, "boom", protocol.SevWarning)}
	}
	writesOn := GitPolicy{AllowWrites: true, AllowDestructive: false, AllowPush: false, ProtectedBranches: []string{"main"}}

	tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{all: diags}, nil, nil,
		func() string { return "claude-code" }, func() GitPolicy { return writesOn })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"brief"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := len(out); n > 1536 {
		t.Fatalf("brief render = %d bytes, want <= 1536:\n%s", n, out)
	}
	if !strings.Contains(out, briefOrientationFooter) {
		t.Errorf("brief render must carry the closing pointer to detail:\"full\"\n%s", out)
	}
	if !strings.Contains(out, "Diagnostics: 40") {
		t.Errorf("want a diagnostics COUNT (40), got:\n%s", out)
	}
	if strings.Contains(out, "boom") {
		t.Errorf("brief render must never carry diagnostic bodies\n%s", out)
	}
	if !strings.Contains(out, "+7 more") {
		t.Errorf("want the 15 memory names capped with a '+7 more' tail, got:\n%s", out)
	}
	if strings.Contains(out, "a fairly long description") {
		t.Errorf("brief render must never carry memory descriptions\n%s", out)
	}
	if !strings.Contains(out, "Edit lane") {
		t.Errorf("Claude Code brief render must still carry the edit-lane rule\n%s", out)
	}
}

// TestSessionStartBrief_FullUnchanged proves adding the brief path left the
// full packet byte-for-byte the same: an explicit detail:"full" call and a
// bare call on a connection with no auto-brief signal (no session_id) must
// render identically, and must still carry every full-only section the card
// says stays full-only.
func TestSessionStartBrief_FullUnchanged(t *testing.T) {
	ws := briefGitInit(t)
	writeBriefMemory(t, ws, "alpha")
	writesOn := GitPolicy{AllowWrites: true}
	newTool := func() *SessionStart {
		return NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, func() GitPolicy { return writesOn })
	}

	bare, err := newTool().Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute bare: %v", err)
	}
	explicit, err := newTool().Execute(context.Background(), json.RawMessage(`{"detail":"full"}`))
	if err != nil {
		t.Fatalf("Execute detail:full: %v", err)
	}
	if bare != explicit {
		t.Fatalf("bare call and explicit detail:\"full\" must render identically\nbare:\n%s\nexplicit:\n%s", bare, explicit)
	}
	for _, want := range []string{"## Memories", "## Tool guidance", "alpha", "a fairly long description"} {
		if !strings.Contains(bare, want) {
			t.Errorf("full render missing full-only content %q:\n%s", want, bare)
		}
	}
	if strings.Contains(bare, briefOrientationFooter) {
		t.Errorf("full render must not carry the brief footer\n%s", bare)
	}
}

// TestSessionStartBrief_AutoBriefOnRepeatSessionID drives the auto-brief
// signal exactly as SessionStart.Execute derives it: a session_id that
// WithExternalID resolves to a non-empty inherited name means this daemon
// already saw this session_id within the 24h grace window (session.FindEnded
// — see WithExternalID's own doc comment), so the default flips to brief
// with no explicit detail argument. First contact (externalIDFn returns "",
// the "no match" case) must default to full, matching the card's "bootstrap
// discoverability must not thin out" trap.
func TestSessionStartBrief_AutoBriefOnRepeatSessionID(t *testing.T) {
	ws := briefGitInit(t)

	t.Run("repeat session_id auto-briefs", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "resumed-session" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"cc-conv-1"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, briefOrientationFooter) {
			t.Errorf("a repeat session_id (inherited name) must default to brief, got:\n%s", out)
		}
	})

	t.Run("first contact stays full", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"cc-conv-2"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, briefOrientationFooter) {
			t.Errorf("first contact (no inherited name) must default to full, got:\n%s", out)
		}
	})

	t.Run("no session_id at all stays full", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, briefOrientationFooter) {
			t.Errorf("an unlinked call must default to full, got:\n%s", out)
		}
	})
}

// TestSessionStartBrief_CarriesIdentitySignals is a mutate-and-confirm-red
// guard for a defect a review round caught: the brief early-return in Execute
// happened before writeSessionIdentity, silently dropping three loud signals
// full always carries — the PR #181 re-pin announcement, the #316
// why-no-server note, and the resumed session's peer-addressable name. All
// three must survive in brief too, on the very call that combines a re-pin
// with an auto-brief-triggering session_id.
func TestSessionStartBrief_CarriesIdentitySignals(t *testing.T) {
	attached := briefGitInit(t)
	target := briefGitInit(t)
	const skipNote = "LSP skipped: the workspace root is the home directory"

	tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(func(_ context.Context, ws, _ string, _ bool) (string, error) { return ws, nil }).
		WithExternalID(func(string) string { return "resumed-session" }).
		WithLSPSkipNote(func() string { return skipNote })

	// workspace re-pins the connection; session_id resolves to a non-empty
	// inherited name, which is exactly the auto-brief trigger — no explicit
	// detail argument, so this must still default to brief.
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+target+`","session_id":"cc-conv-1"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, briefOrientationFooter) {
		t.Fatalf("expected this call to auto-brief, got:\n%s", out)
	}
	if want := "Re-pinned this connection: " + attached + " → " + target; !strings.Contains(out, want) {
		t.Errorf("brief must carry the re-pin announcement %q, got:\n%s", want, out)
	}
	if !strings.Contains(out, "Session:  resumed-session (resumed)") {
		t.Errorf("brief must carry the resumed session's peer-addressable name, got:\n%s", out)
	}
	if !strings.Contains(out, skipNote) {
		t.Errorf("brief must carry the LSP skip note, got:\n%s", out)
	}
	if n := len(out); n > 1536 {
		t.Errorf("brief render with identity signals = %d bytes, want <= 1536:\n%s", n, out)
	}
}

// TestSessionStartBrief_ExplicitDetailWins proves an explicit `detail` always
// overrides the auto-brief default in both directions.
func TestSessionStartBrief_ExplicitDetailWins(t *testing.T) {
	ws := briefGitInit(t)

	t.Run("explicit full overrides auto-brief", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "resumed-session" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"cc-conv-1","detail":"full"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, briefOrientationFooter) {
			t.Errorf("explicit detail:\"full\" must win over the auto-brief default, got:\n%s", out)
		}
	})

	t.Run("explicit brief overrides first-contact default", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"cc-conv-2","detail":"brief"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, briefOrientationFooter) {
			t.Errorf("explicit detail:\"brief\" must win over the first-contact full default, got:\n%s", out)
		}
	})
}

// TestSessionStartBrief_InvalidDetailValue proves a nonsense `detail` value
// is a loud caller-side error, not a silent fallback to full.
func TestSessionStartBrief_InvalidDetailValue(t *testing.T) {
	tool := NewSessionStart(func(context.Context) string { return "/tmp" }, nil, nil, nil, func() string { return "" }, nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"detail":"summary"}`))
	if err == nil {
		t.Fatal("expected an error for an invalid detail value")
	}
	if !strings.Contains(err.Error(), "detail must be") {
		t.Errorf("error = %v, want it to name the valid detail values", err)
	}
}

// TestSessionStartBrief_JoinNamesCap pins joinBriefNames' shape directly:
// under the cap it is a plain comma join, over it a "+N more" tail — the
// mechanism the byte-budget test relies on indirectly.
func TestSessionStartBrief_JoinNamesCap(t *testing.T) {
	under := []string{"a", "b", "c"}
	if got := joinBriefNames(under); got != "a, b, c" {
		t.Errorf("under cap: got %q", got)
	}
	over := make([]string, 0, maxListedBriefNames+3)
	for i := range maxListedBriefNames + 3 {
		over = append(over, fmt.Sprintf("n%d", i))
	}
	got := joinBriefNames(over)
	if !strings.HasSuffix(got, "+3 more") {
		t.Errorf("over cap: got %q, want a '+3 more' tail", got)
	}
	if strings.Count(got, ",") != maxListedBriefNames {
		t.Errorf("over cap: got %q, want exactly %d commas (n shown names + the tail)", got, maxListedBriefNames)
	}
}
