package cli

import (
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// capsClient is a stubClient that reports a fixed set of server capabilities,
// so the resolved-mode negotiation can be exercised without a real server.
type capsClient struct {
	*stubClient
	caps *protocol.ServerCapabilities
}

func (c *capsClient) Capabilities() *protocol.ServerCapabilities { return c.caps }

func pullCaps() *protocol.ServerCapabilities {
	return &protocol.ServerCapabilities{
		DiagnosticProvider: &protocol.BoolOrOptions{Enabled: true},
	}
}

// resolveRequestedDiagnosticsMode maps the config value to the mode plumb
// REQUESTS at initialize: empty/auto defer to the adapter policy (push today),
// explicit push/pull are honoured verbatim.
func TestResolveRequestedDiagnosticsMode(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", diagModePush},
		{"auto", diagModePush},
		{"push", diagModePush},
		{"pull", diagModePull},
	}
	for _, c := range cases {
		if got := resolveRequestedDiagnosticsMode(c.configured, "go"); got != c.want {
			t.Errorf("resolveRequestedDiagnosticsMode(%q) = %q, want %q", c.configured, got, c.want)
		}
	}
}

// autoDiagnosticsMode returns push for every language today (evidence-gated
// future change lives in this one function).
func TestAutoDiagnosticsMode_PushForEveryLanguage(t *testing.T) {
	for _, lang := range []string{"go", "python", "rust", "swift", "zig", "typescript", "kotlin", "html", "java"} {
		if got := autoDiagnosticsMode(lang); got != diagModePush {
			t.Errorf("autoDiagnosticsMode(%q) = %q, want push", lang, got)
		}
	}
}

// resolveDiagMode covers three of the four vocabulary outcomes at Initialize
// time; the fourth ("hybrid") is a later transition covered separately.
func TestResolveDiagMode_Outcomes(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		caps      *protocol.ServerCapabilities
		want      string
	}{
		{"push requested stays push regardless of caps", diagModePush, pullCaps(), diagModePush},
		{"pull requested + server advertises provider", diagModePull, pullCaps(), diagModePull},
		{"pull requested + no provider degrades", diagModePull, &protocol.ServerCapabilities{}, diagModePullUnavailable},
		{"pull requested + nil caps degrades", diagModePull, nil, diagModePullUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
			e := &poolEntry{root: "/x", language: "go"}
			ad := &capsClient{stubClient: &stubClient{}, caps: c.caps}
			p.resolveDiagMode(e, ad, c.requested)
			if e.diagMode != c.want {
				t.Errorf("diagMode = %q, want %q", e.diagMode, c.want)
			}
		})
	}
}

// A -32601 downgrade is sticky across a hibernation wake: poolOnStart re-runs
// resolveDiagMode on every wake with the same surviving poolEntry, and the
// downgrade must keep it in push mode rather than resolving back to pull,
// re-pulling, failing with -32601 again, and re-warning once per wake. A genuine
// restart builds a fresh entry, which re-negotiates from config.
func TestResolveDiagMode_DowngradeStickyAcrossWake(t *testing.T) {
	p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
	root, lang := "/x", "go"
	e := &poolEntry{root: root, language: lang}
	p.entries[poolKey{root, lang}] = e
	ad := &capsClient{stubClient: &stubClient{}, caps: pullCaps()}

	// Initial negotiation: pull requested + server advertises provider → pull.
	p.resolveDiagMode(e, ad, diagModePull)
	if e.diagMode != diagModePull {
		t.Fatalf("initial diagMode = %q, want pull", e.diagMode)
	}

	// A pull returns -32601 → sticky downgrade to push.
	p.downgradeDiagMode(root, lang)
	if e.diagMode != diagModePush || !e.diagDowngraded {
		t.Fatalf("after downgrade: diagMode=%q downgraded=%v, want push/true", e.diagMode, e.diagDowngraded)
	}

	// Simulate a hibernation wake: poolOnStart re-runs resolveDiagMode with the
	// same (surviving) entry and the same config-derived pull request. It must
	// stay push and NOT resolve back to pull.
	p.resolveDiagMode(e, ad, diagModePull)
	if e.diagMode != diagModePush {
		t.Errorf("after wake: diagMode = %q, want push (downgrade must be sticky)", e.diagMode)
	}
	if !e.diagDowngraded {
		t.Errorf("the sticky-downgrade flag must survive a wake")
	}

	// A genuine restart builds a fresh poolEntry (daemon restart / reap+reacquire,
	// or an explicit server restart that clears the flag): negotiation resolves
	// pull again from config.
	fresh := &poolEntry{root: root, language: lang}
	p.resolveDiagMode(fresh, ad, diagModePull)
	if fresh.diagMode != diagModePull {
		t.Errorf("fresh entry diagMode = %q, want pull (a genuine restart re-negotiates)", fresh.diagMode)
	}
}

// diagnosticsHybridFlip flips a "pull" connection to "hybrid" the first time a
// pushed publishDiagnostics is observed, and leaves every other mode untouched.
func TestDiagnosticsHybridFlip(t *testing.T) {
	t.Run("pull flips to hybrid on push", func(t *testing.T) {
		p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
		e := &poolEntry{root: "/x", language: "go", diagMode: diagModePull}
		flip := p.diagnosticsHybridFlip(e)
		flip(protocol.MethodPublishDiagnostics, nil)
		if e.diagMode != diagModeHybrid {
			t.Fatalf("diagMode = %q, want hybrid", e.diagMode)
		}
		// Idempotent: a second push keeps hybrid.
		flip(protocol.MethodPublishDiagnostics, nil)
		if e.diagMode != diagModeHybrid {
			t.Errorf("diagMode = %q, want hybrid after second push", e.diagMode)
		}
	})

	t.Run("push mode is not disturbed by push notifications", func(t *testing.T) {
		p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
		e := &poolEntry{root: "/x", language: "go", diagMode: diagModePush}
		p.diagnosticsHybridFlip(e)(protocol.MethodPublishDiagnostics, nil)
		if e.diagMode != diagModePush {
			t.Errorf("diagMode = %q, want push (a push server in push mode is not hybrid)", e.diagMode)
		}
	})

	t.Run("a non-diagnostics notification never flips", func(t *testing.T) {
		p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
		e := &poolEntry{root: "/x", language: "go", diagMode: diagModePull}
		p.diagnosticsHybridFlip(e)("window/logMessage", nil)
		if e.diagMode != diagModePull {
			t.Errorf("diagMode = %q, want pull (unrelated notification must not flip)", e.diagMode)
		}
	})
}

// clearEntryPullState drops a pool entry's pull-diagnostics result IDs and
// snapshots (the seam poolOnStart runs when a server process (re)starts), and is
// nil-safe for an entry that never attached an Invalidator.
func TestClearEntryPullState(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)
	uri := "file:///x/a.go"
	inv.RecordPullFull(uri, "rid-1", []protocol.Diagnostic{{Message: "boom"}})
	if _, ok := inv.PullResultID(uri); !ok {
		t.Fatal("precondition: expected a stored pull result ID")
	}

	e := &poolEntry{root: "/x", language: "go", inv: inv}
	clearEntryPullState(e)

	if _, ok := inv.PullResultID(uri); ok {
		t.Error("clearEntryPullState must drop the pull result ID")
	}
	if ok := inv.RecordPullUnchanged(uri, "rid-1"); ok {
		t.Error("after clear, a formerly-known result ID must no longer match")
	}

	// Nil-safe: an entry with no Invalidator must not panic.
	clearEntryPullState(&poolEntry{root: "/y", language: "go"})
}

// diagModeFor reads the resolved mode of a pooled entry under the pool lock,
// and returns "" for an entry that is not pooled.
func TestDiagModeFor(t *testing.T) {
	p := &workspacePool{entries: make(map[poolKey]*poolEntry)}
	p.entries[poolKey{"/x", "go"}] = &poolEntry{root: "/x", language: "go", diagMode: diagModePull}
	if got := p.diagModeFor("/x", "go"); got != diagModePull {
		t.Errorf("diagModeFor(/x, go) = %q, want pull", got)
	}
	if got := p.diagModeFor("/nope", "go"); got != "" {
		t.Errorf("diagModeFor(absent) = %q, want empty", got)
	}
}
