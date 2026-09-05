package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// newOverrideSession builds a session whose pool holds exactly the given active
// languages, so languageOverrideErr can be exercised without starting a server.
func newOverrideSession(t *testing.T, active ...string) *connSession {
	t.Helper()
	langs := make([]langConfig, 0, len(active))
	for _, name := range active {
		langs = append(langs, langConfig{name: name, cfg: config.LSPConfig{Enabled: true}})
	}
	return &connSession{
		store: config.NewStore(config.Defaults()),
		ctx:   context.Background(),
		pool:  &workspacePool{langs: langs, entries: make(map[poolKey]*poolEntry)},
	}
}

// TestLanguageOverride_ActiveLanguageAccepted is the control: the whole point of
// refusing the others is that a real one still passes silently.
func TestLanguageOverride_ActiveLanguageAccepted(t *testing.T) {
	s := newOverrideSession(t, "go", "python")
	if err := s.languageOverrideErr("python"); err != nil {
		t.Fatalf("an active language must be accepted, got %v", err)
	}
}

// TestLanguageOverride_UnknownLanguageNamesTheKnownOnes covers a typo. The
// remedy has to be the list, because the caller's mental model of the key space
// is exactly what is wrong.
func TestLanguageOverride_UnknownLanguageNamesTheKnownOnes(t *testing.T) {
	s := newOverrideSession(t, "go")
	err := s.languageOverrideErr("pyhton")
	if err == nil {
		t.Fatal("expected a refusal for a language with no [lsp.<lang>] adapter")
	}
	if !strings.Contains(err.Error(), "pyhton") {
		t.Errorf("refusal %q should quote the language asked for", err)
	}
	for _, want := range []string{"go", "python", "typescript"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q should list the known language %q", err, want)
		}
	}
}

// TestLanguageOverride_DisabledLanguageSaysSoAndHow is the reported case: the
// adapter exists and its markers are even present, but [lsp.<lang>] enabled is
// false, so it never joins the active set. Silently ignoring that is what let a
// repo answer "HTML" to an agent that asked for something else.
func TestLanguageOverride_DisabledLanguageSaysSoAndHow(t *testing.T) {
	s := newOverrideSession(t, "go")
	cfg := s.store.Current()
	ts := cfg.LSP["typescript"]
	ts.Enabled = false
	cfg.LSP["typescript"] = ts
	s.store = config.NewStore(cfg)

	err := s.languageOverrideErr("typescript")
	if err == nil {
		t.Fatal("expected a refusal for a configured-but-inactive language")
	}
	if !strings.Contains(err.Error(), "disabled in config") {
		t.Errorf("refusal %q should distinguish disabled from not-installed", err)
	}
	if !strings.Contains(err.Error(), "enabled = true") {
		t.Errorf("refusal %q should name the config knob to change", err)
	}
}

// TestLanguageOverride_UninstalledLanguageNamesTheBinary is the other inactive
// case, and it needs a different remedy from the disabled one: no config edit
// will help until the server is on PATH.
func TestLanguageOverride_UninstalledLanguageNamesTheBinary(t *testing.T) {
	s := newOverrideSession(t, "go")
	cfg := s.store.Current()
	cfg.LSP["rust"] = config.LSPConfig{
		Command: "definitely-not-installed-rust-analyzer",
		Enabled: true,
	}
	s.store = config.NewStore(cfg)

	err := s.languageOverrideErr("rust")
	if err == nil {
		t.Fatal("expected a refusal for an enabled but uninstalled language")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("refusal %q should say the server is not installed", err)
	}
	if !strings.Contains(err.Error(), "definitely-not-installed-rust-analyzer") {
		t.Errorf("refusal %q should name the binary to install", err)
	}
}

// TestLanguageOverride_NoPoolIsNotARefusal keeps the degraded path open: a
// session with no pool wired has nothing to validate against, and must not turn
// that into a refusal of a legitimate call.
func TestLanguageOverride_NoPoolIsNotARefusal(t *testing.T) {
	s := &connSession{store: config.NewStore(config.Defaults()), ctx: context.Background()}
	if err := s.languageOverrideErr("python"); err != nil {
		t.Fatalf("a session with no pool must not refuse, got %v", err)
	}
}

// TestRepinWorkspace_InactiveLanguageOverrideIsRefused pins the WIRING, not just
// the helper. Mutation-checked: reverting conn_repin.go to the silent-drop
// shape leaves every helper-level test above green, because they call
// languageOverrideErr directly. Only a test that goes through repinWorkspace can
// tell "the refusal exists" from "the refusal is reached".
func TestRepinWorkspace_InactiveLanguageOverrideIsRefused(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	root := freshTempDir(t)
	mustGitDir(t, root)

	// detectTestPool's active set does not include this language, so the
	// override cannot be honoured — and must not be silently discarded.
	_, err := s.repinWorkspace(context.Background(), "file://"+root, "cobol", false)
	if err == nil {
		t.Fatal("expected the re-pin to be refused rather than silently ignoring the language")
	}
	if !strings.Contains(err.Error(), "cobol") {
		t.Errorf("refusal %q should quote the language that was asked for", err)
	}
	if s.workspace() == root {
		t.Error("a refused re-pin must not move the pin")
	}
}

// TestRepinWorkspace_ActiveLanguageOverrideStillPins is the control for the test
// above: the refusal must not have closed the path it guards.
func TestRepinWorkspace_ActiveLanguageOverrideStillPins(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	root := freshTempDir(t)
	mustGitDir(t, root)

	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "go", false); err != nil {
		t.Fatalf("an active language override must still pin: %v", err)
	}
	if got := s.workspace(); got != root {
		t.Errorf("workspace = %q, want %q", got, root)
	}
}

// TestLanguageOverride_ConfigActiveButPoolStaleSaysEnableLsp covers the fourth
// case, which the pre-refusal code could not express at all and which an
// adversarial review found untested.
//
// The pool's effective language set is resolved at construction and widened by
// `plumb enable-lsp`; the config can move underneath it. So a language can be
// enabled AND installed while this daemon has not picked it up. Reporting
// lspActiveStatus there prints the flatly self-contradicting "not active — yes
// (installed)" and sends the caller to edit config that is already correct.
func TestLanguageOverride_ConfigActiveButPoolStaleSaysEnableLsp(t *testing.T) {
	s := newOverrideSession(t, "go") // pool carries go only
	cfg := s.store.Current()
	// Enabled, and its "binary" is one that certainly exists on PATH, so
	// lspActive(cfg) is true while the pool still knows nothing about it.
	cfg.LSP["python"] = config.LSPConfig{Command: "sh", Enabled: true}
	s.store = config.NewStore(cfg)

	err := s.languageOverrideErr("python")
	if err == nil {
		t.Fatal("expected a refusal: the pool has not picked this language up yet")
	}
	if !strings.Contains(err.Error(), "enable-lsp python") {
		t.Errorf("refusal %q should point at the live enable, not a config edit", err)
	}
	if strings.Contains(err.Error(), "disabled in config") || strings.Contains(err.Error(), "not installed") {
		t.Errorf("refusal %q must not report a state that is not true", err)
	}
}
