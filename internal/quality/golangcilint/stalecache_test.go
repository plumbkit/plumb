package golangcilint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/quality"
)

// touch creates an empty regular file at dir/name and returns its path.
func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func finding(file string) quality.Finding {
	return quality.Finding{File: file, Line: 7, Severity: quality.SeverityError, Code: "errcheck", Message: "unchecked error", Source: "golangci-lint"}
}

// flagStaleCache must fire ONLY when every finding names an absent file. The
// mixed case is the dangerous one: a genuine finding alongside phantoms from a
// deleted worktree must survive untouched.
func TestFlagStaleCache_Table(t *testing.T) {
	base := t.TempDir()
	touch(t, base, "real.go")
	touch(t, base, "other.go")
	realAbs := filepath.Join(base, "real.go")
	if err := os.Mkdir(filepath.Join(base, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A path inside a deleted sibling worktree: the directory itself is gone.
	ghostAbs := filepath.Join(t.TempDir(), "gone-worktree", "internal", "x.go")

	cases := []struct {
		name      string
		findings  []quality.Finding
		wantStale bool
	}{
		{"empty findings", nil, false},
		{"empty non-nil findings", []quality.Finding{}, false},
		{"all real, relative paths", []quality.Finding{finding("real.go"), finding("other.go")}, false},
		{"all real, absolute paths", []quality.Finding{finding(realAbs)}, false},
		{"all phantom, relative paths", []quality.Finding{finding("ghost.go"), finding("also-ghost.go")}, true},
		{"all phantom, absolute paths", []quality.Finding{finding(ghostAbs)}, true},
		{"all phantom, same path twice", []quality.Finding{finding("ghost.go"), finding("ghost.go")}, true},
		{"mixed: real first", []quality.Finding{finding("real.go"), finding("ghost.go")}, false},
		{"mixed: phantom first", []quality.Finding{finding("ghost.go"), finding("real.go")}, false},
		{"mixed: absolute real among phantoms", []quality.Finding{finding("ghost.go"), finding(realAbs), finding("g2.go")}, false},
		// A path that exists but is a directory still EXISTS. Conservative
		// rule: only "does not exist" counts as phantom.
		{"path exists but is a directory", []quality.Finding{finding("adir")}, false},
		{"directory among phantoms", []quality.Finding{finding("ghost.go"), finding("adir")}, false},
		// A finding with no filename cannot be checked, so it must not be
		// counted as missing.
		{"empty filename", []quality.Finding{finding("")}, false},
		{"empty filename among phantoms", []quality.Finding{finding("ghost.go"), finding("")}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagStaleCache(tc.findings, base, "golangci-lint")
			stale := len(got) == 1 && got[0].Code == StaleCacheCode
			if stale != tc.wantStale {
				t.Fatalf("stale-cache signal = %v, want %v (got %d finding(s): %+v)", stale, tc.wantStale, len(got), got)
			}
			if !tc.wantStale {
				// Non-signal cases must pass the findings through byte for byte.
				if len(got) != len(tc.findings) {
					t.Fatalf("got %d finding(s), want the original %d", len(got), len(tc.findings))
				}
				for i := range tc.findings {
					if got[i] != tc.findings[i] {
						t.Errorf("finding %d mutated: got %+v, want %+v", i, got[i], tc.findings[i])
					}
				}
			}
		})
	}
}

// The replacement finding must tell the agent exactly what to run, and how many
// findings it stands in for.
func TestFlagStaleCache_MessageIsActionable(t *testing.T) {
	base := t.TempDir()
	got := flagStaleCache([]quality.Finding{finding("a.go"), finding("b.go"), finding("c.go")}, base, "golangci-lint")
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want exactly 1", len(got))
	}
	f := got[0]
	if f.Source != "golangci-lint" {
		t.Errorf("Source = %q, want %q", f.Source, "golangci-lint")
	}
	if f.Severity != quality.SeverityWarning {
		t.Errorf("Severity = %v, want warning", f.Severity)
	}
	if !strings.Contains(f.Message, StaleCacheHint) {
		t.Errorf("message does not name the remediation %q: %s", StaleCacheHint, f.Message)
	}
	if !strings.Contains(f.Message, "3") {
		t.Errorf("message does not say how many findings it replaced: %s", f.Message)
	}
}

// An empty base means "the process working directory", which is what os.Stat
// already resolves a relative path against. Joining an empty base would produce
// a rooted path and call every relative finding phantom.
func TestResolveFindingPath(t *testing.T) {
	cases := []struct {
		name, path, base, want string
	}{
		{"relative joins base", "x.go", "/w/pkg", "/w/pkg/x.go"},
		{"relative with parent segment", "../other/x.go", "/w/pkg", "/w/other/x.go"},
		{"absolute ignores base", "/elsewhere/x.go", "/w/pkg", "/elsewhere/x.go"},
		{"empty base passes through", "x.go", "", "x.go"},
		{"empty path passes through", "", "/w/pkg", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveFindingPath(tc.path, tc.base); got != tc.want {
				t.Errorf("resolveFindingPath(%q, %q) = %q, want %q", tc.path, tc.base, got, tc.want)
			}
		})
	}
}

// Relative resolution against the real working directory, exercised through
// os.Stat rather than string comparison: a file that exists only under base must
// be seen as present when base is supplied, and absent when it is not.
func TestAllPathsMissing_ResolvesRelativeToBase(t *testing.T) {
	base := t.TempDir()
	touch(t, base, "real.go")
	fs := []quality.Finding{finding("real.go")}

	if allPathsMissing(fs, base) {
		t.Error("a file that exists under base was reported missing — relative paths are not being resolved against base")
	}
	if !allPathsMissing(fs, filepath.Join(base, "nope")) {
		t.Error("resolution is not using base at all: the file was found under a base that does not contain it")
	}
}

// fakeLinter installs a stub golangci-lint that prints jsonOut on stdout and
// exits 1, as the real binary does when it has issues to report.
func fakeLinter(t *testing.T, jsonOut string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "golangci-lint")
	body := "#!/bin/sh\ncat <<'PLUMBEOF'\n" + jsonOut + "\nPLUMBEOF\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	orig := lookPath
	lookPath = func(string) (string, error) { return script, nil }
	t.Cleanup(func() { lookPath = orig })
}

func issuesJSON(filenames ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"Issues":[`)
	for i, f := range filenames {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"FromLinter":"errcheck","Text":"Error return value is not checked",` +
			`"Severity":"error","Pos":{"Filename":"` + f + `","Line":12,"Column":2}}`)
	}
	sb.WriteString("]}")
	return sb.String()
}

// End-to-end through Analyse: golangci-lint output naming only files inside a
// deleted worktree must come back as the single stale-cache signal.
func TestAnalyse_StaleCacheSignalReachesFindings(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	fakeLinter(t, issuesJSON("removed-worktree/internal/a.go", "removed-worktree/internal/b.go"))

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want the single stale-cache signal: %+v", len(got), got)
	}
	if got[0].Code != StaleCacheCode {
		t.Fatalf("Code = %q, want %q", got[0].Code, StaleCacheCode)
	}
	if !strings.Contains(got[0].Message, StaleCacheHint) {
		t.Errorf("message does not name the remediation: %s", got[0].Message)
	}
}

// The other half of the same seam, and the assertion that catches a broken
// relative-path resolution: golangci-lint reports paths relative to its working
// directory (cmd.Dir), so a finding naming the file just written must be seen as
// real and passed straight through.
func TestAnalyse_RealRelativePathIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	fakeLinter(t, issuesJSON("live.go"))

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1", len(got))
	}
	if got[0].Code != "errcheck" {
		t.Fatalf("a finding in a file that exists was rewritten to %q — the existence check resolved a live path as phantom", got[0].Code)
	}
	if got[0].File != "live.go" || got[0].Line != 12 {
		t.Errorf("finding was mutated: %+v", got[0])
	}
}

// A real finding sitting alongside phantoms must not be swallowed.
func TestAnalyse_MixedFindingsPassThrough(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	fakeLinter(t, issuesJSON("removed-worktree/a.go", "live.go", "removed-worktree/b.go"))

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d finding(s), want all 3 passed through: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Code == StaleCacheCode {
			t.Fatalf("a mixed findings set was reported as a stale cache, hiding a real finding: %+v", got)
		}
	}
}
