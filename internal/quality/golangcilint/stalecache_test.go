package golangcilint

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/quality"
)

// touch creates a file at dir/name and returns its ABSOLUTE path.
func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
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
	realAbs := touch(t, base, "real.go")
	otherAbs := touch(t, base, "other.go")
	dirAbs := filepath.Join(base, "adir")
	if err := os.Mkdir(dirAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Paths inside a deleted sibling worktree: the directory itself is gone.
	ghostAbs := filepath.Join(t.TempDir(), "gone-worktree", "internal", "x.go")
	ghost2Abs := filepath.Join(t.TempDir(), "gone-worktree", "internal", "y.go")

	cases := []struct {
		name      string
		findings  []quality.Finding
		wantStale bool
	}{
		{"empty findings", nil, false},
		{"empty non-nil findings", []quality.Finding{}, false},
		{"all real", []quality.Finding{finding(realAbs), finding(otherAbs)}, false},
		{"all phantom", []quality.Finding{finding(ghostAbs), finding(ghost2Abs)}, true},
		{"all phantom, same path twice", []quality.Finding{finding(ghostAbs), finding(ghostAbs)}, true},
		{"mixed: real first", []quality.Finding{finding(realAbs), finding(ghostAbs)}, false},
		{"mixed: phantom first", []quality.Finding{finding(ghostAbs), finding(realAbs)}, false},
		// A path that exists but is a directory still EXISTS. Conservative
		// rule: only "does not exist" counts as phantom.
		{"path exists but is a directory", []quality.Finding{finding(dirAbs)}, false},
		{"directory among phantoms", []quality.Finding{finding(ghostAbs), finding(dirAbs)}, false},
		// A finding with no filename cannot be checked, so it must not be
		// counted as missing.
		{"empty filename", []quality.Finding{finding("")}, false},
		{"empty filename among phantoms", []quality.Finding{finding(ghostAbs), finding("")}, false},
		// RELATIVE paths mean the --path-mode=abs fallback ran (an older
		// golangci-lint). There is no unambiguous base to resolve them
		// against, so the check must decline rather than guess. These two
		// names do not exist under any base, and must STILL pass through.
		{"relative path alone", []quality.Finding{finding("sub/deep/nope.go")}, false},
		{"relative among phantoms", []quality.Finding{finding(ghostAbs), finding("sub/deep/nope.go")}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagStaleCache(tc.findings, "golangci-lint")
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
	gone := filepath.Join(t.TempDir(), "gone")
	got := flagStaleCache([]quality.Finding{
		finding(filepath.Join(gone, "a.go")),
		finding(filepath.Join(gone, "b.go")),
		finding(filepath.Join(gone, "c.go")),
	}, "golangci-lint")
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
	// The count must be the number of findings replaced, not any digit that
	// happens to appear in the sentence.
	if !strings.Contains(f.Message, "suppressed 3 finding(s)") {
		t.Errorf("message does not say it replaced 3 findings: %s", f.Message)
	}
}

// A stat that fails for a reason OTHER than "does not exist" must count as
// present: a check that cannot see a file must never claim the file is gone.
// Driven through the stat seam so the assertion is exact on every platform and
// does not depend on the test process being unprivileged.
func TestPathPresent_OnlyNotExistCountsAsMissing(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantPresent bool
	}{
		{"stat succeeds", nil, true},
		{"does not exist", fs.ErrNotExist, false},
		{"permission denied", fs.ErrPermission, true},
		{"some other error", fs.ErrInvalid, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubStat(t, func(string) (os.FileInfo, error) { return nil, tc.err })
			if got := pathPresent(filepath.Join(t.TempDir(), "x.go")); got != tc.wantPresent {
				t.Errorf("pathPresent = %v, want %v for stat error %v", got, tc.wantPresent, tc.err)
			}
		})
	}
}

// The same rule against the real filesystem: a file under a parent the process
// cannot traverse must not be reported gone.
func TestPathPresent_PermissionDeniedParentIsPresent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — permission bits do not deny access")
	}
	parent := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "real.go")
	if err := os.WriteFile(target, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if _, err := os.Stat(target); err == nil {
		t.Skip("stat still succeeds despite mode 000 — filesystem does not enforce it here")
	}
	if !pathPresent(target) {
		t.Error("a file whose parent denies access was reported missing; only fs.ErrNotExist may count as missing")
	}
}

// stubStat replaces the stat seam for the duration of the test.
func stubStat(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	orig := statFile
	statFile = fn
	t.Cleanup(func() { statFile = orig })
}

// Distinct paths are stat'd once each, however many findings name them.
func TestAllPathsMissing_StatsEachDistinctPathOnce(t *testing.T) {
	var calls []string
	stubStat(t, func(p string) (os.FileInfo, error) {
		calls = append(calls, p)
		return nil, fs.ErrNotExist
	})

	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")
	if !allPathsMissing([]quality.Finding{finding(a), finding(b), finding(a), finding(b), finding(a)}) {
		t.Fatal("all paths were stubbed missing, want allPathsMissing = true")
	}
	if len(calls) != 2 {
		t.Errorf("stat called %d time(s) for 2 distinct paths across 5 findings: %v", len(calls), calls)
	}
}

// fakeLinter installs a stub golangci-lint that prints jsonOut on stdout and
// exits 1, as the real binary does when it has issues to report.
func fakeLinter(t *testing.T, jsonOut string) {
	t.Helper()
	fakeLinterScript(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$PLUMB_ARGS\"\ncat <<'PLUMBEOF'\n"+jsonOut+"\nPLUMBEOF\nexit 1\n")
}

// fakeLinterScript installs an arbitrary stub script as golangci-lint. Each
// invocation appends its argv to $PLUMB_ARGS; read it back with readArgs.
func fakeLinterScript(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "golangci-lint")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PLUMB_ARGS", filepath.Join(dir, "args.txt"))

	orig := lookPath
	lookPath = func(string) (string, error) { return script, nil }
	t.Cleanup(func() { lookPath = orig })
}

// readArgs returns the argv the stub linter was invoked with so far.
func readArgs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("PLUMB_ARGS"))
	if err != nil {
		t.Fatalf("stub linter recorded no argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
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

// Analyse must ask for absolute paths. Without the flag there is no single
// directory a relative path can be resolved against — see pathModeAbs.
func TestAnalyse_RequestsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	fakeLinter(t, issuesJSON(src))

	if _, err := New().Analyse(t.Context(), []string{src}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args := readArgs(t); !slices.Contains(args, pathModeAbs) {
		t.Errorf("golangci-lint was invoked without %s: %v", pathModeAbs, args)
	}
}

// End-to-end through Analyse: golangci-lint output naming only files inside a
// deleted worktree must come back as the single stale-cache signal.
func TestAnalyse_StaleCacheSignalReachesFindings(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	gone := filepath.Join(t.TempDir(), "removed-worktree", "internal")
	fakeLinter(t, issuesJSON(filepath.Join(gone, "a.go"), filepath.Join(gone, "b.go")))

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

// A finding in a file that EXISTS must pass through untouched. The file sits
// BELOW the directory golangci-lint runs in, which is the shape that broke the
// original implementation: it resolved output paths against cmd.Dir, so a path
// measured from the module root doubled up and stat'd as missing.
func TestAnalyse_RealFindingBelowRunDirIsNotFlagged(t *testing.T) {
	root := t.TempDir()
	src := touch(t, root, filepath.Join("sub", "deep", "live.go"))
	fakeLinter(t, issuesJSON(src))

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1: %+v", len(got), got)
	}
	if got[0].Code != "errcheck" {
		t.Fatalf("a finding in a file that exists was rewritten to %q — a real finding was destroyed", got[0].Code)
	}
	if got[0].File != src || got[0].Line != 12 {
		t.Errorf("finding was mutated: %+v", got[0])
	}
}

// A real finding sitting alongside phantoms must not be swallowed.
func TestAnalyse_MixedFindingsPassThrough(t *testing.T) {
	root := t.TempDir()
	src := touch(t, root, filepath.Join("sub", "deep", "live.go"))
	gone := filepath.Join(t.TempDir(), "removed-worktree")
	fakeLinter(t, issuesJSON(filepath.Join(gone, "a.go"), src, filepath.Join(gone, "b.go")))

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

// An older golangci-lint (before v2.1.0) rejects --path-mode and exits without
// writing anything. Analyse must retry without the flag rather than lose every
// finding, and must then leave the relative paths it gets back alone.
func TestAnalyse_RetriesWithoutPathModeOnOlderBinary(t *testing.T) {
	root := t.TempDir()
	src := touch(t, root, filepath.Join("sub", "deep", "live.go"))

	// Rejects --path-mode like an older binary; otherwise reports one issue
	// with a module-root-relative path, which is what such a binary emits.
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$PLUMB_ARGS\"\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in --path-mode*) echo 'unknown flag: --path-mode' >&2; exit 3;; esac\n" +
		"done\n" +
		"cat <<'PLUMBEOF'\n" + issuesJSON("sub/deep/live.go") + "\nPLUMBEOF\nexit 1\n"
	fakeLinterScript(t, body)

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want the retry to recover 1: %+v", len(got), got)
	}
	if got[0].Code != "errcheck" {
		t.Fatalf("Code = %q — the fallback path suppressed a real finding it cannot resolve", got[0].Code)
	}

	args := readArgs(t)
	if !slices.Contains(args, pathModeAbs) {
		t.Errorf("first invocation did not try %s: %v", pathModeAbs, args)
	}
	// The retry must have happened: "run" appears once per invocation.
	if n := strings.Count(strings.Join(args, "\n"), "run"); n < 2 {
		t.Errorf("golangci-lint was invoked once, want a retry without %s: %v", pathModeAbs, args)
	}
}

// A run that fails with no output on BOTH attempts yields no findings rather
// than an error — the write has already succeeded.
func TestAnalyse_TotalFailureYieldsNoFindings(t *testing.T) {
	dir := t.TempDir()
	src := touch(t, dir, "live.go")
	fakeLinterScript(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$PLUMB_ARGS\"\necho boom >&2\nexit 3\n")

	got, err := New().Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d finding(s), want none: %+v", len(got), got)
	}
}
