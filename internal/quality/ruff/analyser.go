// Package ruff implements a quality.Analyser that shells out to ruff, the
// Python linter. If ruff cannot be found the analyser returns no findings rather
// than erroring — the write still succeeds — but it says so in the log exactly
// once, for the reason the golangci-lint adapter records: a silently disabled
// feature is worse than a noisy one.
//
// The resolution fallback matters more here than anywhere. ruff is normally
// installed with `uv tool install` or `pip install --user`, both of which put it
// in ~/.local/bin — a directory the daemon's inherited PATH routinely lacks, so
// a PATH-only lookup would present as "plumb does not support ruff" on a machine
// where ruff works fine in the shell. See quality.LookBinary.
package ruff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/quality"
)

// name is the registry spelling, and the Source stamped on every finding.
const name = "ruff"

// Analyser runs ruff on Python source files.
// Concurrency: Analyse may be called concurrently; each call is independent.
type Analyser struct {
	// bin is the [quality.bin] override, empty when the user set none.
	bin string
}

// New returns a new ruff Analyser. binOverride is the [quality.bin] entry for
// ruff, or "" to resolve it the ordinary way.
func New(binOverride string) *Analyser { return &Analyser{bin: binOverride} }

func (*Analyser) Name() string { return name }

// Supports reports whether path is a Python source file. The extension list
// lives in the registry so the Settings pane and the analyser cannot disagree
// about which files a tool owns.
func (*Analyser) Supports(path string) bool {
	t, ok := quality.ToolByName(name)
	return ok && t.SupportsPath(path)
}

// forceExclude makes ruff honour the project's own `exclude` settings for files
// named explicitly on the command line.
//
// Without it, ruff's documented behaviour is that an explicitly-passed path
// overrides exclusion — sensible for someone typing `ruff check generated.py`,
// wrong for plumb, which passes an explicit path for every write. A project that
// excludes its generated or vendored Python would get post-write findings for
// exactly the files it told its linter to ignore, and nothing in plumb's output
// would explain why its findings disagreed with the project's own `ruff check`.
const forceExclude = "--force-exclude"

// Analyse runs ruff on files and returns parsed findings. Returns (nil, nil) if
// ruff is not resolvable, it fails to run, or it reports nothing.
func (a *Analyser) Analyse(ctx context.Context, files []string) ([]quality.Finding, error) {
	if len(files) == 0 {
		return nil, nil
	}
	t, ok := quality.ToolByName(name)
	if !ok {
		return nil, nil // unreachable: the registry always carries this row
	}
	bin, found := quality.LookBinary(t, a.bin)
	if !found {
		logUnavailableOnce(ctx, t)
		return nil, nil // binary absent — skip, but not silently (logged once)
	}

	// ruff discovers pyproject.toml / ruff.toml / .ruff.toml by walking up from
	// the files it is given, but its `exclude` globs and `src` entries resolve
	// against the working directory. The daemon runs from "/", so anchoring at
	// the analysed file's directory is what makes a project's own configuration
	// apply — the same reason the golangci-lint adapter anchors its run.
	var dir string
	if d := filepath.Dir(files[0]); filepath.IsAbs(d) {
		dir = d
	}

	stdout, stderr, runErr := run(ctx, bin, dir, files)

	// ruff exits 1 when it has violations, which is a successful run; anything
	// else with a non-zero exit is a real failure and is reported even when
	// stdout parsed, because a run that lints three files, fails on one and
	// emits partial JSON would otherwise hand back a short findings list with
	// nothing anywhere to say it was short. Silence about a partial result is
	// the same failure mode as silence about a missing binary.
	if runErr != nil && !isViolationsExit(runErr) {
		slog.WarnContext(ctx, "ruff exited with an error", "error", runErr, "stderr", stderr,
			"partial_output", len(stdout) > 0)
	}

	// A successful run always writes a JSON document to stdout — "[]" when there
	// is nothing to report — so empty stdout means ruff failed to run rather
	// than a clean file. Surface that instead of reporting "no findings", which
	// is the shape of the bug this whole package keeps having.
	if len(stdout) == 0 {
		return nil, nil
	}
	return parseOutput(stdout), nil
}

// run performs one ruff invocation, returning stdout, a bounded tail of stderr,
// and the process error. ruff exits 1 when it finds violations and 2 on a real
// failure; neither is an error for us — the caller parses stdout regardless of
// exit code, because a run that found violations is a successful run.
func run(ctx context.Context, bin, dir string, files []string) ([]byte, string, error) {
	args := make([]string, 0, len(files)+4)
	args = append(args, "check", "--output-format=json", forceExclude, "--")
	args = append(args, files...)

	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderrTail(stderr.String()), err
}

// violationsExit is the status ruff uses for "I found something", as opposed to
// "I could not run". Treating it as a failure would log a warning on every
// Python write that has any finding at all, which is how a warning stops being
// read.
const violationsExit = 1

// isViolationsExit reports whether err is ruff's ordinary exit-1-with-findings.
func isViolationsExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == violationsExit
}

// stderrTail returns the trailing portion of stderr, bounded, for diagnostics.
func stderrTail(s string) string {
	const maxLen = 512
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		s = "…" + s[len(s)-maxLen:]
	}
	return s
}

// diagnostic is one element of ruff's JSON array. The field names and their
// nullability are taken from ruff's own JsonDiagnostic serializer
// (crates/ruff_db/src/diagnostic/render/json.rs), not from a sample of one
// machine's output. Fields plumb does not use (name, severity, fix, noqa_row,
// end_location, cell) are omitted; encoding/json ignores them.
//
// Not consumed, and deliberately: ruff emits a `severity`, but outside preview
// mode its renderer hardcodes it to "error" for every diagnostic, so reading it
// would flatten a style lint and a syntax error onto the same level. plumb
// derives severity from whether a rule code is present instead — see parseOutput.
type diagnostic struct {
	// Code is a pointer because null and "" mean different things: ruff's
	// serializer types it Option<&str>, and in preview mode a diagnostic with no
	// rule (a syntax error) carries null rather than an empty string. Outside
	// preview it always carries the diagnostic's id, so the null branch is
	// defensive on stable ruff and load-bearing on preview.
	Code    *string `json:"code"`
	Message string  `json:"message"`
	// Filename and Location are Option in ruff too, and a null Location is the
	// reason this is a value struct rather than a pointer: encoding/json treats
	// null into a struct as a no-op, leaving row 0, which is exactly the "line
	// unknown" Finding.Line documents and Runner.format already renders without a
	// line number.
	Filename string  `json:"filename"`
	URL      *string `json:"url"`
	Location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	} `json:"location"`
}

// syntaxErrorCode is the Code plumb stamps on a ruff diagnostic that carries
// none. Rendering a null code as an empty string would print
// "  L3 : invalid syntax", which reads as a formatting bug rather than as the
// most important thing ruff can tell you.
const syntaxErrorCode = "syntax-error"

// parseOutput converts ruff's JSON array into findings. Malformed output yields
// no findings rather than an error: the write has already succeeded and a linter
// we cannot parse must not fail it.
func parseOutput(data []byte) []quality.Finding {
	var diags []diagnostic
	// Decode only the leading JSON value, so anything ruff appends after the
	// document (a summary line on a future version, say) cannot cost us every
	// finding the way json.Unmarshal's trailing-data error would.
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&diags); err != nil {
		return nil
	}
	findings := make([]quality.Finding, 0, len(diags))
	for _, d := range diags {
		code, sev := syntaxErrorCode, quality.SeverityError
		if d.Code != nil {
			code, sev = *d.Code, quality.SeverityWarning
		}
		findings = append(findings, quality.Finding{
			File:     d.Filename,
			Line:     d.Location.Row,
			Column:   d.Location.Column,
			Severity: sev,
			Code:     code,
			Message:  d.Message,
			Source:   name,
		})
	}
	return findings
}

// unavailableOnce bounds the "not found" log to one line per daemon lifetime:
// the analyser runs on every Python write, so an unconditional warning would
// flood the log, and that is precisely how a warning ends up being ignored.
var unavailableOnce sync.Once

func logUnavailableOnce(ctx context.Context, t quality.Tool) {
	unavailableOnce.Do(func() {
		slog.InfoContext(ctx, "quality: ruff not found — post-write Python quality findings are disabled",
			"searched", append([]string{"PATH"}, quality.BinDirs(t.Ecosystem)...),
			"hint", "install ruff (astral.sh/ruff), set [quality.bin] ruff, "+
				"or put its directory on the PATH the daemon inherits")
	})
}
