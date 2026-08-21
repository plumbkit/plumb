package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/stats"
)

// stubDiagnostics implements diagnosticsSource for tests.
type stubDiagnostics struct {
	all map[string][]protocol.Diagnostic
}

func (s *stubDiagnostics) Diagnostics(uri string) []protocol.Diagnostic     { return s.all[uri] }
func (s *stubDiagnostics) AllDiagnostics() map[string][]protocol.Diagnostic { return s.all }
func (s *stubDiagnostics) Tracked(uri string) bool                          { _, ok := s.all[uri]; return ok }

// stubTimedDiagnostics implements timedDiagnosticsSource so the diagnostics
// section can exercise the staleness annotation.
type stubTimedDiagnostics struct {
	all   map[string][]protocol.Diagnostic
	times map[string]time.Time
}

func (s *stubTimedDiagnostics) Diagnostics(uri string) []protocol.Diagnostic     { return s.all[uri] }
func (s *stubTimedDiagnostics) AllDiagnostics() map[string][]protocol.Diagnostic { return s.all }
func (s *stubTimedDiagnostics) Tracked(uri string) bool                          { _, ok := s.all[uri]; return ok }
func (s *stubTimedDiagnostics) AllDiagnosticTimes() map[string]time.Time         { return s.times }

// TestSessionStart_DiagnosticsStalenessNote verifies the orientation packet
// flags a diagnostic whose file mtime is newer than its last analysis — the
// "stale errors from in-flight work" case — when the source reports times.
func TestSessionStart_DiagnosticsStalenessNote(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stale*.go")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := f.Name()
	f.Close()
	uri := "file://" + path

	src := &stubTimedDiagnostics{
		all: map[string][]protocol.Diagnostic{
			uri: {makeDiag(0, 0, "stale boom", protocol.SevError)},
		},
		// Analysis predates the file's current mtime → stale.
		times: map[string]time.Time{uri: time.Now().Add(-2 * time.Second)},
	}

	ss := &SessionStart{diag: src}
	var sb strings.Builder
	ss.writeSessionDiagnostics(&sb)
	out := sb.String()
	if !strings.Contains(out, "stale boom") {
		t.Fatalf("expected the diagnostic message in output:\n%s", out)
	}
	if !strings.Contains(out, "modified") {
		t.Fatalf("expected a staleness note in output:\n%s", out)
	}
}

// TestWriteSessionStats_IncludesP95 verifies the per-tool stats line now carries
// p95 alongside the average.
func TestWriteSessionStats_IncludesP95(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	for i := range 5 {
		if err := db.Record(stats.Call{
			SessionID: "s", Workspace: "/ws", Tool: "edit_file",
			CalledAt: now, DurationMs: int64(100 + i*10), Success: true,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var sb strings.Builder
	(&SessionStart{}).writeSessionStats(&sb, "/ws")
	out := sb.String()
	if !strings.Contains(out, "Most-used tools") {
		t.Fatalf("missing stats header:\n%s", out)
	}
	if !strings.Contains(out, "p95") {
		t.Fatalf("expected p95 in the tool line:\n%s", out)
	}
}

// TestSessionStart_NoLSPGuidance covers recognised projects whose language
// server is not attached. Must never claim "LSP is ready" and must name
// the concrete next step (opt-in knob or binary-path guidance).
func TestSessionStart_NoLSPGuidance(t *testing.T) {
	// run creates a workspace with one marker file and asserts the output
	// contains wantStr and does not claim LSP is available.
	run := func(t *testing.T, markerFile, markerContent, wantStr string) {
		t.Helper()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, markerFile), []byte(markerContent), 0o644); err != nil {
			t.Fatalf("write %s: %v", markerFile, err)
		}
		tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{all: nil}, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "LSP is ready") {
			t.Errorf("must not claim LSP is ready\n%s", out)
		}
		if !strings.Contains(out, wantStr) {
			t.Errorf("want %q in output\n%s", wantStr, out)
		}
	}

	t.Run("java/maven names opt-in knob", func(t *testing.T) {
		run(t, "pom.xml", "<project/>", "[lsp.java]")
	})
	t.Run("swift names opt-in knob", func(t *testing.T) {
		run(t, "Package.swift", "// swift-tools-version:5.9", "[lsp.swift]")
	})
	t.Run("zig names opt-in knob", func(t *testing.T) {
		run(t, "build.zig", "const std = @import(\"std\");", "[lsp.zig]")
	})
	t.Run("kotlin/settings.gradle.kts names opt-in knob", func(t *testing.T) {
		run(t, "settings.gradle.kts", "rootProject.name = \"app\"", "[lsp.kotlin]")
	})
	t.Run("typescript/tsconfig names opt-in knob", func(t *testing.T) {
		run(t, "tsconfig.json", "{}", "[lsp.typescript]")
	})
	// Go adapter ships on-by-default: the message explains the binary is likely not installed.
	t.Run("go names binary-path guidance not opt-in knob", func(t *testing.T) {
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{all: nil}, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "LSP is ready") {
			t.Errorf("must not claim LSP is ready\n%s", out)
		}
		if !strings.Contains(out, "isn't installed") {
			t.Errorf("want binary-path guidance for Go (on-by-default adapter)\n%s", out)
		}
		if strings.Contains(out, "[lsp.go]") {
			t.Errorf("Go should not show opt-in knob (it ships enabled)\n%s", out)
		}
	})
}

func TestSessionStart_RecommendedFirstStep(t *testing.T) {
	// writes a minimal go.mod so detectLanguage returns "Go" for the temp workspace.
	makeGoWorkspace := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		return ws
	}

	t.Run("active errors suggest diagnostics", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: map[string][]protocol.Diagnostic{
			"file:///ws/main.go": {makeDiag(0, 0, "undefined: foo", protocol.SevError)},
		}}
		tool := NewSessionStart(func(context.Context) string { return ws }, diag, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "Active errors detected") {
			t.Errorf("want 'Active errors detected' in output\n%s", out)
		}
	})

	t.Run("LSP available no errors suggests workspace_symbols", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: nil}
		tool := NewSessionStart(func(context.Context) string { return ws }, diag, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "go" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "workspace_symbols") {
			t.Errorf("want 'workspace_symbols' in output\n%s", out)
		}
	})

	t.Run("no LSP with Go language names binary path guidance", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		// No LSP attached, no topology — topology is wired but returns nil store.
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "LSP is ready") {
			t.Errorf("must not claim LSP is ready\n%s", out)
		}
		// Go adapter is on-by-default, so the message should explain it's likely not installed.
		if !strings.Contains(out, "isn't installed") {
			t.Errorf("want binary-path guidance for Go\n%s", out)
		}
		if !strings.Contains(out, "find_files") {
			t.Errorf("want fallback mention of find_files\n%s", out)
		}
	})

	// PLAN-258: a workspace with no detectable primary language never acquires
	// one, so the recommended step told the agent no language server was attached
	// — while per-file routing was answering its queries. Believing it, the agent
	// stopped asking.
	t.Run("no primary but routed names the LSP tools", func(t *testing.T) {
		ws := t.TempDir()
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "" }).
			WithLSPRouted(func() []string { return []string{"html"} })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "No language server is attached") {
			t.Errorf("claims no language server is attached while one is serving html\n%s", out)
		}
		for _, want := range []string{"html", "get_definition", "find_references"} {
			if !strings.Contains(out, want) {
				t.Errorf("want %q in the recommended step\n%s", want, out)
			}
		}
	})

	t.Run("an attached primary outranks the routed advisory", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{}, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "go" }).
			WithLSPRouted(func() []string { return []string{"html"} })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "LSP is ready") {
			t.Errorf("an attached primary must still lead the recommended step\n%s", out)
		}
	})

	t.Run("no LSP no language uses default", func(t *testing.T) {
		ws := t.TempDir()
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "find_files") {
			t.Errorf("want 'find_files' in output\n%s", out)
		}
	})

	t.Run("warning-only diags still suggest workspace_symbols", func(t *testing.T) {
		ws := makeGoWorkspace(t)
		diag := &stubDiagnostics{all: map[string][]protocol.Diagnostic{
			"file:///ws/main.go": {makeDiag(1, 0, "unused variable", protocol.SevWarning)},
		}}
		tool := NewSessionStart(func(context.Context) string { return ws }, diag, nil, nil, func() string { return "" }, nil).
			WithLSPLanguage(func() string { return "go" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "workspace_symbols") {
			t.Errorf("want 'workspace_symbols' (not error path) in output\n%s", out)
		}
	})
}

func TestSessionStart_WorkspaceScale(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "util.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write util.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// 4 files total, 2 Go (.go) — go.mod is not a .go file
	if !strings.Contains(out, "Scale:") {
		t.Errorf("want 'Scale:' in output\n%s", out)
	}
	if !strings.Contains(out, "Go") {
		t.Errorf("want 'Go' in Scale line\n%s", out)
	}
}

func TestSessionStart_ClientNameGuidance(t *testing.T) {
	// Verifies that Claude Code tool guidance is emitted for exact "claude-code"
	// and version-qualified "claude-code/<ver>" matches (case-insensitive),
	// but NOT for names that merely share the prefix (e.g. "claude-codegen").
	tests := []struct {
		name         string
		clientName   string
		wantGuidance bool
	}{
		{"exact lowercase", "claude-code", true},
		{"exact uppercase", "Claude-Code", true},
		{"mixed case", "CLAUDE-CODE", true},
		{"version qualified", "claude-code/1.2.3", true},
		{"version qualified mixed case", "Claude-Code/2.0.0", true},
		{"claude desktop", "claude-desktop", false},
		{"empty string", "", false},
		{"unrelated client", "vscode", false},
		{"prefix only similar", "claude", false},
		{"false positive guard", "claude-codegen", false},
		{"false positive guard mixed case", "Claude-Codegen", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.clientName
			tool := NewSessionStart(
				func(context.Context) string { return t.TempDir() },
				nil,
				nil,
				nil,
				func() string { return name },
				nil,
			)

			out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}

			hasGuidance := strings.Contains(out, "## Tool guidance (Claude Code)")
			if hasGuidance != tc.wantGuidance {
				t.Errorf("clientName=%q: wantGuidance=%v got=%v\noutput:\n%s",
					tc.clientName, tc.wantGuidance, hasGuidance, out)
			}
		})
	}
}

// TestSessionStart_DesktopGuidance verifies the Desktop guidance fires for the
// real Claude Desktop client name ("claude-ai", not "claude-desktop") and
// leads with the workspace-pinning instruction.
func TestSessionStart_DesktopGuidance(t *testing.T) {
	for _, name := range []string{"claude-ai", "claude-ai/0.1.0", "claude-desktop"} {
		t.Run(name, func(t *testing.T) {
			tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return name }, nil)
			out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !strings.Contains(out, "## Tool guidance (Claude Desktop)") {
				t.Errorf("clientName=%q: want Desktop guidance\n%s", name, out)
			}
			if !strings.Contains(out, "Pin your project first") {
				t.Errorf("clientName=%q: want workspace-pinning instruction\n%s", name, out)
			}
		})
	}
}

// TestSessionStart_LanguageOverride verifies the `language` arg forces the
// primary language on the current workspace (via the repin callback), shows the
// forced language in the identity line even when no root marker named it, and
// does not announce a project re-pin (no workspace switch happened).
func TestSessionStart_LanguageOverride(t *testing.T) {
	attached := t.TempDir()
	var gotWs, gotLang string
	tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil).
		WithLSPLanguage(func() string { return "swift" }). // server attached after the forced pin
		WithRepin(func(_ context.Context, ws, lang string, _ bool) (string, error) {
			gotWs, gotLang = ws, lang
			return ws, nil
		})
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"language":"swift"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotLang != "swift" {
		t.Errorf("repin received language %q, want swift", gotLang)
	}
	if gotWs != attached {
		t.Errorf("repin received workspace %q, want the current %q", gotWs, attached)
	}
	if !strings.Contains(out, "Language: Swift") {
		t.Errorf("display should show the forced Swift primary\n%s", out)
	}
	if strings.Contains(out, "Re-pinned this connection") {
		t.Errorf("a language-only pin must not announce a project re-pin\n%s", out)
	}
}

// TestSessionStart_WorkspaceResolution covers the resolution chain after the
// os.Getwd() phantom was removed: the daemon's attached root wins, an explicit
// arg is honoured only when nothing is attached, and nothing-resolves errors
// (no daemon-cwd guess).
func TestSessionStart_WorkspaceResolution(t *testing.T) {
	t.Run("mismatch without repin callback falls back to error", func(t *testing.T) {
		attached := t.TempDir()
		var conflict string
		tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil).
			WithPinConflict(func(requested string) { conflict = requested })
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"/some/other/path"}`))
		if err == nil {
			t.Fatal("want mismatch error when explicit workspace differs from attached root, got nil")
		}
		if !strings.Contains(err.Error(), "already pinned") {
			t.Errorf("error should mention pinned workspace, got: %v", err)
		}
		if conflict != "/some/other/path" {
			t.Fatalf("pin conflict callback = %q, want requested workspace", conflict)
		}
	})

	t.Run("explicit arg re-pins when repin callback wired", func(t *testing.T) {
		attached := t.TempDir()
		target := t.TempDir()
		var got string
		tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil).
			WithRepin(func(_ context.Context, ws, _ string, _ bool) (string, error) {
				got = ws
				return ws, nil
			})
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+target+`"}`))
		if err != nil {
			t.Fatalf("Execute should re-pin, got error: %v", err)
		}
		if got != target {
			t.Fatalf("repin callback received %q, want %q", got, target)
		}
		if !strings.Contains(out, "# Workspace: "+target) {
			t.Errorf("output should show the new workspace %q\n%s", target, out)
		}
		if !strings.Contains(out, "Re-pinned this connection: "+attached+" → "+target) {
			t.Errorf("output should announce the re-pin\n%s", out)
		}
	})

	t.Run("attached root returned when explicit arg matches", func(t *testing.T) {
		attached := t.TempDir()
		tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+attached+`"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "# Workspace: "+attached) {
			t.Errorf("attached root should be returned; want %q\n%s", attached, out)
		}
	})

	t.Run("explicit arg used when nothing attached", func(t *testing.T) {
		explicit := t.TempDir()
		tool := NewSessionStart(func(context.Context) string { return "" }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+explicit+`"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "# Workspace: "+explicit) {
			t.Errorf("explicit arg should resolve; want %q\n%s", explicit, out)
		}
	})

	t.Run("nothing resolves errors (no cwd guess)", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return "" }, nil, nil, nil, func() string { return "" }, nil)
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatal("want noWorkspaceError when nothing resolves, got nil")
		}
	})
}

// TestSameDir_TrailingSlash verifies that a path with a trailing slash resolves
// to the same directory as the canonical form, so it does not trigger a re-pin.
func TestSameDir_TrailingSlash(t *testing.T) {
	attached := t.TempDir()
	trailing := attached + "/"
	tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+trailing+`"}`))
	if err != nil {
		t.Fatalf("trailing slash should not trigger re-pin; got error: %v", err)
	}
	if !strings.Contains(out, "# Workspace: "+attached) {
		t.Errorf("expected attached root in output; got:\n%s", out)
	}
}

// TestSameDir_SymlinkAlias verifies that a symlink pointing at the attached
// workspace root is treated as the same directory (sameDir uses os.SameFile).
func TestSameDir_SymlinkAlias(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink creation failed (likely Windows without privilege): %v", err)
	}
	tool := NewSessionStart(func(context.Context) string { return realDir }, nil, nil, nil, func() string { return "" }, nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+linkDir+`"}`))
	if err != nil {
		t.Fatalf("symlink alias should not trigger re-pin; got error: %v", err)
	}
	if !strings.Contains(out, "# Workspace: "+realDir) {
		t.Errorf("expected realDir %q in output; got:\n%s", realDir, out)
	}
}

// TestFormatGitPolicy covers the pure policy formatter: the shell-avoidance
// steer appears only when writes are enabled, and the "trust it over any cached
// note" line is always present (it is the line that contradicts a stale
// "git is read-only" memory at the point of orientation).
func TestFormatGitPolicy(t *testing.T) {
	const trust = "trust it over any cached note"
	const steer = "commit through the `git` tool, not the shell"
	tests := []struct {
		name        string
		policy      GitPolicy
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:   "default: writes on, destructive/push off",
			policy: GitPolicy{AllowWrites: true, ProtectedBranches: []string{"main", "master"}},
			wantContain: []string{
				"Commits & staging ENABLED", steer,
				"Destructive (reset/checkout/rebase): off.",
				"Push/fetch/pull: off.",
				"Protected branches: main, master.",
				trust,
			},
		},
		{
			name:   "all gates on",
			policy: GitPolicy{AllowWrites: true, AllowDestructive: true, AllowPush: true, ProtectedBranches: []string{"main"}},
			wantContain: []string{
				"Destructive (reset/checkout/rebase): on.",
				"Push/fetch/pull: on.",
				"Protected branches: main.",
				trust,
			},
		},
		{
			name:        "writes disabled",
			policy:      GitPolicy{AllowWrites: false},
			wantContain: []string{"Read-only", "`[git] allow_writes = false`", trust},
			wantAbsent:  []string{"Commits & staging ENABLED", steer},
		},
		{
			name:        "writes on, no protected branches",
			policy:      GitPolicy{AllowWrites: true},
			wantContain: []string{"Commits & staging ENABLED", trust},
			wantAbsent:  []string{"Protected branches:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatGitPolicy(tc.policy)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("want %q in:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("did not want %q in:\n%s", absent, got)
				}
			}
		})
	}
}

// TestSessionStart_GitPolicySection verifies the section is wired into Execute:
// rendered inside a git repo when the policy is wired, and omitted both when
// gitPolicyFn is nil and when the workspace is not a git repo.
func TestSessionStart_GitPolicySection(t *testing.T) {
	const header = "## Git (via the `git` tool"
	writesOn := func() GitPolicy {
		return GitPolicy{AllowWrites: true, ProtectedBranches: []string{"main", "master"}}
	}
	gitInit := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		if out, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
			t.Skipf("git init unavailable: %v (%s)", err, out)
		}
		return ws
	}

	t.Run("rendered in a git repo when policy wired", func(t *testing.T) {
		ws := gitInit(t)
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, writesOn)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, header) {
			t.Errorf("want git policy section in a git repo\n%s", out)
		}
		if !strings.Contains(out, "Commits & staging ENABLED") {
			t.Errorf("want ENABLED policy body\n%s", out)
		}
	})

	t.Run("omitted when gitPolicyFn is nil", func(t *testing.T) {
		ws := gitInit(t)
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, header) {
			t.Errorf("git policy section should be omitted when gitPolicyFn is nil\n%s", out)
		}
	})

	t.Run("omitted outside a git repo", func(t *testing.T) {
		// A path with no git repo above it: `git -C <missing> branch` errors, so
		// gitBranch returns "" and the section is gated off. t.TempDir() alone
		// won't do — in this repo the test temp root lives inside the worktree,
		// so git would resolve the enclosing plumb repo and report a branch.
		ws := filepath.Join(t.TempDir(), "no-such-dir")
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, writesOn)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, header) {
			t.Errorf("git policy section should be omitted outside a git repo\n%s", out)
		}
	})
}

// TestSessionStart_LeanProfileNote checks the lean note is emitted (with the
// hidden count and resolution reason folded in, no tool enumeration) and that
// lean guidance never steers the agent to a tool hidden from tools/list.
func TestSessionStart_LeanProfileNote(t *testing.T) {
	s := &SessionStart{
		clientNameFn: func() string { return "claude-code" },
		toolProfile:  func() (string, int, string) { return "lean", 33, "verified-deferred-discovery" },
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	out := sb.String()

	if !strings.Contains(out, "Tool profile: lean") {
		t.Errorf("lean note missing:\n%s", out)
	}
	if !strings.Contains(out, "33 commodity tools hidden") {
		t.Errorf("lean note should state the hidden count:\n%s", out)
	}
	if !strings.Contains(out, "(reason: verified-deferred-discovery)") {
		t.Errorf("lean note should state the resolution reason:\n%s", out)
	}
	// The note must not enumerate hidden tool names.
	for _, hidden := range []string{"call_hierarchy", "type_hierarchy", "topology_routes", "topology_impact", "explain_symbol"} {
		if strings.Contains(out, hidden) {
			t.Errorf("lean guidance recommends hidden tool %q:\n%s", hidden, out)
		}
	}
}

// TestSessionStart_FullProfileNote verifies the compact full-profile note
// ("Tool profile: full (reason: ...)") is emitted when a profile accessor is
// wired and resolves to "full" with a non-empty reason, and that it never
// carries the lean-specific wording.
func TestSessionStart_FullProfileNote(t *testing.T) {
	s := &SessionStart{
		clientNameFn: func() string { return "claude-code" },
		toolProfile:  func() (string, int, string) { return "full", 0, "schema-discovery-only-client" },
	}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	out := sb.String()
	if !strings.Contains(out, "Tool profile: full (reason: schema-discovery-only-client)") {
		t.Errorf("full profile note missing or malformed:\n%s", out)
	}
	if strings.Contains(out, "Tool profile: lean") {
		t.Error("full profile should not emit the lean note")
	}
}

// TestSessionStart_UnwiredProfileSilent verifies that a SessionStart with no
// tool-profile accessor wired at all (the pre-Task-3 default) emits no profile
// output whatsoever — preserving legacy behaviour for callers that never wire
// WithToolProfile.
func TestSessionStart_UnwiredProfileSilent(t *testing.T) {
	s := &SessionStart{clientNameFn: func() string { return "claude-code" }}
	var sb strings.Builder
	s.writeSessionGuidance(&sb)
	if strings.Contains(sb.String(), "Tool profile:") {
		t.Errorf("no tool-profile accessor wired: expected no profile output:\n%s", sb.String())
	}
}

func TestSessionStart_PurposeValidAndPersisted(t *testing.T) {
	ws := t.TempDir()
	var got string
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithPurpose(func(p string) { got = p })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"purpose":"deploy-fix"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "deploy-fix" {
		t.Fatalf("persisted purpose = %q, want deploy-fix", got)
	}
}

func TestSessionStart_PurposeInvalidRejected(t *testing.T) {
	ws := t.TempDir()
	called := false
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithPurpose(func(string) { called = true })
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"purpose":"bad purpose!"}`))
	if err == nil {
		t.Fatalf("expected error for invalid purpose")
	}
	if !strings.Contains(err.Error(), "invalid purpose") {
		t.Fatalf("error = %v, want it to mention invalid purpose", err)
	}
	if called {
		t.Fatalf("purpose callback must not run for an invalid value")
	}
}

func TestSessionStart_EmptyPurposeIsNoOp(t *testing.T) {
	ws := t.TempDir()
	called := false
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithPurpose(func(string) { called = true })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if called {
		t.Fatalf("purpose callback must not run when no purpose supplied")
	}
}

// TestSessionStart_Idempotent proves the tool's own doc-comment claim
// ("Idempotent — safe to call multiple times") for a connection that is
// already pinned: a second Execute call on the same tool, with the same args,
// must still succeed, must report the same workspace without announcing a
// spurious re-pin, and must not duplicate the per-call side effects the tool
// owns — the purpose and external-ID callbacks each fire exactly once per
// Execute call (twice total across the two calls), not accumulating extra
// invocations from the earlier call.
func TestSessionStart_Idempotent(t *testing.T) {
	ws := t.TempDir()
	var purposeCalls, externalIDCalls int
	tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil).
		WithPurpose(func(string) { purposeCalls++ }).
		WithExternalID(func(string) string { externalIDCalls++; return "" })

	args := json.RawMessage(`{"purpose":"deploy-fix","session_id":"abc-123"}`)

	out1, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	out2, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	if !strings.Contains(out1, "# Workspace: "+ws) || !strings.Contains(out2, "# Workspace: "+ws) {
		t.Fatalf("both calls should report the same workspace\ncall 1:\n%s\ncall 2:\n%s", out1, out2)
	}
	if strings.Contains(out2, "Re-pinned this connection") {
		t.Errorf("a second call with an unchanged workspace must not announce a re-pin\n%s", out2)
	}
	if purposeCalls != 2 {
		t.Errorf("purpose callback should fire exactly once per Execute call (2 calls total), got %d", purposeCalls)
	}
	if externalIDCalls != 2 {
		t.Errorf("external-ID callback should fire exactly once per Execute call (2 calls total), got %d", externalIDCalls)
	}
}

// TestSessionStart_ForceThreadedToRepin verifies the `force` arg reaches the
// re-pin callback: absent it arrives false, present it arrives true. The guard
// itself lives daemon-side (conn_stickypin_test.go); the tool only threads it.
func TestSessionStart_ForceThreadedToRepin(t *testing.T) {
	attached := t.TempDir()
	target := t.TempDir()
	var gotForce []bool
	tool := NewSessionStart(func(context.Context) string { return attached }, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(func(_ context.Context, ws, _ string, force bool) (string, error) {
			gotForce = append(gotForce, force)
			return ws, nil
		})
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+target+`"}`)); err != nil {
		t.Fatalf("Execute without force: %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"workspace":"`+target+`","force":true}`)); err != nil {
		t.Fatalf("Execute with force: %v", err)
	}
	if len(gotForce) != 2 || gotForce[0] != false || gotForce[1] != true {
		t.Fatalf("force threading = %v, want [false true]", gotForce)
	}
}

// TestSessionStart_ForceThreadedOnUnattachedLanguagePin covers the other repin
// call site: an unattached connection pinning workspace+language. The guard
// cannot fire there today (no pin is held), but the caller's force must still
// arrive rather than being silently dropped.
func TestSessionStart_ForceThreadedOnUnattachedLanguagePin(t *testing.T) {
	target := t.TempDir()
	var gotForce bool
	tool := NewSessionStart(func(context.Context) string { return "" }, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(func(_ context.Context, _, _ string, force bool) (string, error) {
			gotForce = force
			return target, nil
		})
	raw := json.RawMessage(`{"workspace":"` + target + `","language":"go","force":true}`)
	if _, err := tool.Execute(context.Background(), raw); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !gotForce {
		t.Fatal("force: true must reach the repin callback on the unattached workspace+language path")
	}
}

// TestSessionStart_LSPSkipNoteInIdentity pins issue #316's orientation half: a
// home-directory pin comes back with no language and nothing naming why. When
// the skip-note accessor is wired and returns a note, the identity block must
// carry it verbatim — including alongside a "Language:" line, since a stray
// root marker at $HOME must not read as "a server is attached". Unwired (or
// empty) must render nothing, preserving legacy callers' packets.
func TestSessionStart_LSPSkipNoteInIdentity(t *testing.T) {
	ws := t.TempDir()
	const note = "LSP skipped: the workspace root is the home directory"

	tool := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{all: nil}, nil, nil, func() string { return "" }, nil).
		WithLSPLanguage(func() string { return "" }).
		WithLSPSkipNote(func() string { return note })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, note) {
		t.Errorf("orientation packet must name why no LSP is attached for a home pin\n%s", out)
	}

	tool2 := NewSessionStart(func(context.Context) string { return ws }, &stubDiagnostics{all: nil}, nil, nil, func() string { return "" }, nil)
	out2, err := tool2.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute (no accessor): %v", err)
	}
	if strings.Contains(out2, "LSP skipped") {
		t.Errorf("orientation packet renders an LSP-skip note with no accessor wired\n%s", out2)
	}
}
