package ruff

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/quality"
)

func TestSupports(t *testing.T) {
	a := New("")
	cases := []struct {
		path string
		want bool
	}{
		{"/project/app.py", true},
		{"/project/stubs.pyi", true},
		{"/project/main.go", false},
		{"/project/app.ts", false},
		{"/project/noext", false},
		{"/project/app.py.bak", false},
	}
	for _, tc := range cases {
		if got := a.Supports(tc.path); got != tc.want {
			t.Errorf("Supports(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The document ruff actually emits, abbreviated to the fields plumb reads.
const oneFinding = `[
  {
    "cell": null,
    "code": "F401",
    "end_location": {"column": 10, "row": 1},
    "filename": "/project/app.py",
    "fix": {"applicability": "safe", "edits": [], "message": "Remove unused import"},
    "location": {"column": 8, "row": 1},
    "message": "` + "`os`" + ` imported but unused",
    "noqa_row": 1,
    "url": "https://docs.astral.sh/ruff/rules/unused-import"
  }
]`

func TestParseOutput_Valid(t *testing.T) {
	got := parseOutput([]byte(oneFinding))
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1", len(got))
	}
	f := got[0]
	if f.Code != "F401" {
		t.Errorf("Code = %q, want F401", f.Code)
	}
	if f.File != "/project/app.py" || f.Line != 1 || f.Column != 8 {
		t.Errorf("position = %s:%d:%d, want /project/app.py:1:8", f.File, f.Line, f.Column)
	}
	if !strings.Contains(f.Message, "imported but unused") {
		t.Errorf("Message = %q", f.Message)
	}
	if f.Severity != quality.SeverityWarning {
		t.Errorf("Severity = %v, want warning for an ordinary rule violation", f.Severity)
	}
	if f.Source != "ruff" {
		t.Errorf("Source = %q, want ruff — the findings header is built from it", f.Source)
	}
}

// ruff reports a file it cannot parse with a NULL rule code. Rendering that as
// an empty string would print "L3 : invalid syntax", which reads as a formatting
// bug rather than as the most important thing ruff can tell you — and a syntax
// error is an error, not a style warning.
func TestParseOutput_NullCodeIsASyntaxError(t *testing.T) {
	data := []byte(`[{"code":null,"message":"SyntaxError: Expected an expression",` +
		`"filename":"/project/app.py","location":{"row":3,"column":1},"url":null}]`)
	got := parseOutput(data)
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1", len(got))
	}
	if got[0].Code != syntaxErrorCode {
		t.Errorf("Code = %q, want %q", got[0].Code, syntaxErrorCode)
	}
	if got[0].Severity != quality.SeverityError {
		t.Errorf("Severity = %v, want error for a file that does not parse", got[0].Severity)
	}
}

// ruffSnapshotAllNull is taken verbatim from ruff's own JSON renderer snapshot
// (crates/ruff_db/src/diagnostic/render/json.rs), which is where every field
// name in `diagnostic` comes from. ruff's changelog records that filename,
// location and end_location may be null rather than defaulting to "" and row 1
// - so a nulled document is a real thing plumb will be handed, not a
// hypothetical, and it must yield a usable finding rather than a panic or a
// dropped one.
const ruffSnapshotAllNull = `[
  {
    "cell": null,
    "code": "test-diagnostic",
    "end_location": null,
    "filename": null,
    "fix": null,
    "location": null,
    "message": "main diagnostic message",
    "name": "test-diagnostic",
    "noqa_row": null,
    "severity": "error",
    "url": "https://docs.astral.sh/ruff/rules/test-diagnostic"
  }
]`

func TestParseOutput_NullLocationAndFilenameStillYieldAFinding(t *testing.T) {
	got := parseOutput([]byte(ruffSnapshotAllNull))
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1 - a nulled location must not drop the diagnostic", len(got))
	}
	f := got[0]
	if f.Code != "test-diagnostic" || f.Message != "main diagnostic message" {
		t.Errorf("finding = %+v, want the code and message carried through", f)
	}
	// Line 0 is Finding.Line's documented "unknown", and Runner.format already
	// renders such a finding without a line number rather than printing "L0".
	if f.Line != 0 || f.Column != 0 {
		t.Errorf("null location should leave the position unknown, got %d:%d", f.Line, f.Column)
	}
	if f.File != "" {
		t.Errorf("null filename should leave File empty, got %q", f.File)
	}
}

func TestParseOutput_EmptyArrayIsNoFindings(t *testing.T) {
	if got := parseOutput([]byte(`[]`)); len(got) != 0 {
		t.Errorf("got %d finding(s), want none", len(got))
	}
}

// Malformed output must not fail the write: it has already succeeded, and a
// linter we cannot parse must never turn a good edit into an error.
func TestParseOutput_MalformedYieldsNothing(t *testing.T) {
	for _, data := range []string{"", "not json", `{"unexpected":"object"}`, `[{"code":`} {
		if got := parseOutput([]byte(data)); len(got) != 0 {
			t.Errorf("parseOutput(%q) = %d finding(s), want none", data, len(got))
		}
	}
}

// fakeRuff installs a stub ruff and returns its path, to be handed to New as the
// [quality.bin] override. Each invocation appends its argv to $PLUMB_ARGS.
func fakeRuff(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "ruff")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	t.Setenv("PLUMB_ARGS", filepath.Join(dir, "args.txt"))
	return script
}

func readArgs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("PLUMB_ARGS"))
	if err != nil {
		t.Fatalf("stub ruff recorded no argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func touchPy(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "app.py")
	if err := os.WriteFile(path, []byte("import os\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ruff exits 1 when it has violations to report. That is a SUCCESSFUL run, and
// treating a non-zero exit as failure would discard every finding ruff ever
// produces — the one outcome the feature exists for.
func TestAnalyse_ExitOneWithFindingsIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	src := touchPy(t, dir)
	bin := fakeRuff(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$PLUMB_ARGS\"\ncat <<'PLUMBEOF'\n"+
		oneFinding+"\nPLUMBEOF\nexit 1\n")

	got, err := New(bin).Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want 1 — a violations exit was treated as a failed run", len(got))
	}
}

// --force-exclude is load-bearing, not decorative: plumb passes an explicit path
// for every write, and ruff's documented behaviour is that an explicit path
// overrides the project's own `exclude`. Without the flag a project would get
// post-write findings for exactly the files it told ruff to ignore.
func TestAnalyse_RequestsForceExclude(t *testing.T) {
	dir := t.TempDir()
	src := touchPy(t, dir)
	bin := fakeRuff(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$PLUMB_ARGS\"\necho '[]'\n")

	if _, err := New(bin).Analyse(t.Context(), []string{src}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := readArgs(t)
	if !slices.Contains(args, forceExclude) {
		t.Errorf("ruff was invoked without %s: %v", forceExclude, args)
	}
	if !slices.Contains(args, "--output-format=json") {
		t.Errorf("ruff was invoked without JSON output: %v", args)
	}
	// The `--` guard keeps a path that begins with a dash from being read as a
	// flag; without it a file named -x.py would silently change the invocation.
	if !slices.Contains(args, "--") {
		t.Errorf("ruff was invoked without the -- argument guard: %v", args)
	}
}

// Empty stdout with a non-zero exit is a run that FAILED, not a clean file. The
// distinction matters because reporting "no findings" for a broken invocation is
// how a silently disabled analyser looks from the outside.
func TestAnalyse_TotalFailureYieldsNoFindings(t *testing.T) {
	dir := t.TempDir()
	src := touchPy(t, dir)
	bin := fakeRuff(t, "#!/bin/sh\necho 'ruff failed: unknown option' >&2\nexit 2\n")

	got, err := New(bin).Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d finding(s), want none: %+v", len(got), got)
	}
}

// ruff can emit PARTIAL output: lint several files, fail on one, write the
// findings it did get and exit non-zero. Taking those findings silently is the
// same failure mode as a silently missing binary - the caller gets a short list
// with nothing anywhere to say it was short - so the error is reported even
// though stdout parsed.
func TestAnalyse_PartialFailureIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	src := touchPy(t, dir)
	bin := fakeRuff(t, "#!/bin/sh\ncat <<'PLUMBEOF'\n"+oneFinding+"\nPLUMBEOF\n"+
		"echo 'ruff failed: could not read other.py' >&2\nexit 2\n")

	var mu sync.Mutex
	var msgs []string
	h := &captureHandler{fn: func(msg string) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got, err := New(bin).Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d finding(s), want the 1 ruff did produce", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	var logged bool
	for _, m := range msgs {
		if strings.Contains(m, "ruff exited with an error") {
			logged = true
		}
	}
	if !logged {
		t.Errorf("a partial failure was swallowed; logged: %v", msgs)
	}
}

// The ordinary exit-1-with-violations must NOT log: it happens on every Python
// write that has any finding at all, and a warning that fires constantly is a
// warning nobody reads.
func TestAnalyse_ViolationsExitDoesNotLogAnError(t *testing.T) {
	dir := t.TempDir()
	src := touchPy(t, dir)
	bin := fakeRuff(t, "#!/bin/sh\ncat <<'PLUMBEOF'\n"+oneFinding+"\nPLUMBEOF\nexit 1\n")

	var mu sync.Mutex
	var msgs []string
	h := &captureHandler{fn: func(msg string) {
		mu.Lock()
		msgs = append(msgs, msg)
		mu.Unlock()
	}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := New(bin).Analyse(t.Context(), []string{src}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range msgs {
		if strings.Contains(m, "exited with an error") {
			t.Errorf("a normal violations exit logged an error: %v", msgs)
		}
	}
}

// captureHandler is a minimal slog.Handler that calls fn with every record's
// message; mirrors the equivalent helper in the golangcilint package (unexported
// there, so duplicated rather than shared).
type captureHandler struct {
	fn func(msg string)
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.fn(r.Message)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// A missing binary is skipped, never an error: the write has already succeeded.
func TestAnalyse_MissingBinaryIsNotAnError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	src := touchPy(t, dir)

	got, err := New(filepath.Join(t.TempDir(), "nope", "ruff")).Analyse(t.Context(), []string{src})
	if err != nil {
		t.Fatalf("a missing binary must not error: %v", err)
	}
	if got != nil {
		t.Errorf("want nil findings, got %+v", got)
	}
}

func TestAnalyse_EmptyFiles(t *testing.T) {
	findings, err := New("").Analyse(t.Context(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if findings != nil {
		t.Errorf("expected nil findings for empty file list")
	}
}
