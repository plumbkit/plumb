// Package golangcilint implements a quality.Analyser that shells out to
// golangci-lint. If golangci-lint cannot be found the analyser returns no
// findings rather than erroring — the write still succeeds — but it says so in
// the log exactly once, because a silently disabled feature is worse than a
// noisy one (this bit us: golangci-lint was installed in ~/go/bin, the daemon's
// PATH did not include it, and the post-write quality findings simply never
// appeared, with nothing anywhere to explain why).
package golangcilint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/quality"
)

// Analyser runs golangci-lint on Go source files.
// Concurrency: Analyse may be called concurrently; each call is independent.
type Analyser struct{}

// New returns a new golangci-lint Analyser.
func New() *Analyser { return &Analyser{} }

func (*Analyser) Name() string { return "golangci-lint" }

// Supports reports whether path is a Go source file eligible for linting.
func (*Analyser) Supports(path string) bool {
	ext := filepath.Ext(path)
	return ext == ".go"
}

// pathModeAbs makes golangci-lint report absolute filenames.
//
// This is load-bearing for the stale-cache check, not a cosmetic choice. There
// is NO fixed directory a relative golangci-lint path can be resolved against:
// `run.relative-path-mode` selects between the working directory, the go.mod
// directory, the git root, and the config file's directory, and the DEFAULT is
// itself conditional — `cfg` when a config file is discovered, `wd` when none
// is. Measured with v2.12.2, same module, file at <root>/sub/deep/bad.go:
//
//	no config, cwd=<root>/sub/deep         -> "bad.go"
//	config at <root>, cwd=<root>/sub/deep  -> "sub/deep/bad.go"
//	relative-path-mode: gitroot            -> "<modroot>/sub/deep/bad.go"
//	output.path-prefix: myprefix           -> "myprefix/sub/deep/bad.go"
//
// Resolving against any single base therefore mis-stats real findings for some
// perfectly ordinary project — and a real finding stat'd as missing is exactly
// how this check would suppress findings that matter. --path-mode=abs removes
// the guess: measured across all four relative-path-modes, three working
// directories, with and without a config file, and with output.path-prefix set,
// it reports the same absolute path every time. Findings are never rendered by
// path (quality.Runner.format prints only line, code and message), so this
// changes nothing an agent or user sees.
const pathModeAbs = "--path-mode=abs"

// Analyse runs golangci-lint on files and returns parsed findings.
// Returns (nil, nil) if golangci-lint is not on PATH, the linter fails to
// run, or it reports no issues.
func (a *Analyser) Analyse(ctx context.Context, files []string) ([]quality.Finding, error) {
	if len(files) == 0 {
		return nil, nil
	}
	bin, ok := LookBinary()
	if !ok {
		logUnavailableOnce(ctx)
		return nil, nil // binary absent — skip, but not silently (logged once)
	}

	// golangci-lint resolves the Go module from its working directory. The daemon
	// runs from "/", which is in no module, so anchor the run at the analysed file's
	// directory. files are the absolute paths of just-written source files.
	var dir string
	if d := filepath.Dir(files[0]); filepath.IsAbs(d) {
		dir = d
	}

	stdout, stderr, runErr := runLinter(ctx, bin, dir, files, true)
	// --path-mode landed in golangci-lint v2.1.0; an earlier v2 exits on the
	// unknown flag before writing anything. Retry without it rather than lose
	// every finding — the stale-cache check then declines to run, because the
	// paths come back relative and pathPresent will not judge those.
	if len(stdout) == 0 && runErr != nil {
		stdout, stderr, runErr = runLinter(ctx, bin, dir, files, false)
	}

	// A successful run always writes a JSON document to stdout (even with zero
	// issues), so empty stdout means the linter failed to run rather than a
	// clean file. Surface that instead of silently reporting "no findings".
	if len(stdout) == 0 {
		if runErr != nil {
			slog.WarnContext(ctx, "golangci-lint failed to run", "error", runErr, "stderr", stderr)
		}
		return nil, nil
	}
	return flagStaleCache(parseOutput(stdout, a.Name()), a.Name()), nil
}

// runLinter performs one golangci-lint run, returning its stdout, a bounded
// tail of stderr, and the process error. golangci-lint exits non-zero when it
// finds issues; that is not an error for us — the caller parses stdout
// regardless of exit code.
//
// --output.json.path=stdout is the golangci-lint v2 spelling; the v1
// --out-format=json flag was removed and errors with "unknown flag".
func runLinter(ctx context.Context, bin, dir string, files []string, absPaths bool) ([]byte, string, error) {
	args := make([]string, 0, len(files)+3)
	args = append(args, "run", "--output.json.path=stdout")
	if absPaths {
		args = append(args, pathModeAbs)
	}
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

// StaleCacheCode is the Code carried by the synthetic finding that replaces a
// findings set golangci-lint attributed entirely to files that are not on disk.
const StaleCacheCode = "stale-cache"

// StaleCacheHint is the remediation an agent or human should run. golangci-lint's
// cache is process-global and machine-wide, so plumb reports rather than cleans
// it — see flagStaleCache.
const StaleCacheHint = "golangci-lint cache clean"

// flagStaleCache detects golangci-lint's stale-cache failure mode: after a
// sibling git worktree is deleted, the shared cache keeps returning issues
// attributed to paths inside the removed directory. The agent then debugs a
// "lint regression" in files that do not exist — recorded instances produced 29
// and 9 phantom findings, and the cost is precisely that it looks like a genuine
// regression in the change you just made.
//
// The signal is deliberately ALL-or-nothing: it fires only when EVERY finding
// names a path absent from disk. A mixed set proves the run did make contact
// with the current tree, and announcing "stale cache, ignore everything" while
// one finding is real would be a worse failure than the bug — so a mixed set is
// returned untouched. When every finding is phantom nothing actionable is lost
// by replacing them: no agent can fix a file that does not exist.
//
// plumb reports and does not remediate. `golangci-lint cache clean` mutates
// state shared by every concurrent agent and by the user's own terminal, and the
// re-run it implies is a cold-cache lint pass — far beyond the post-write
// analyser timeout (2 s by default), so auto-remediation would in practice
// degrade into silence while slowing every peer down. The message names the
// command instead.
//
// The check operates only on absolute paths (see pathModeAbs and pathPresent),
// which is what makes it safe: a finding in a file that exists cannot stat as
// missing, so a real finding can never be suppressed. That is a property of the
// construction, not a list of path layouts someone remembered to handle.
func flagStaleCache(findings []quality.Finding, source string) []quality.Finding {
	if len(findings) == 0 || !allPathsMissing(findings) {
		return findings
	}
	return []quality.Finding{{
		Severity: quality.SeverityWarning,
		Code:     StaleCacheCode,
		Source:   source,
		Message: fmt.Sprintf("suppressed %d finding(s): every one names a file that is not on disk, "+
			"which means golangci-lint returned them from a stale cache (typically a deleted sibling "+
			"worktree), not from your code. Run `%s`, then lint again.", len(findings), StaleCacheHint),
	}}
}

// allPathsMissing reports whether every finding names a path absent from disk,
// at one stat per DISTINCT path. The memoisation is not a micro-optimisation:
// the recorded stale-cache incidents carried 29 and 9 findings, and a phantom
// set is typically many findings across few files, so the distinct-path count is
// what the syscall bill should track.
func allPathsMissing(findings []quality.Finding) bool {
	checked := make(map[string]bool, len(findings))
	for _, f := range findings {
		present, seen := checked[f.File]
		if !seen {
			present = pathPresent(f.File)
			checked[f.File] = present
		}
		if present {
			return false
		}
	}
	return true
}

// statFile is the stat seam (tests substitute it).
var statFile = os.Stat

// pathPresent reports whether p names something on disk, erring towards
// "present" in every case it cannot settle. Two rules, both deliberate:
//
// A non-absolute path is always present. plumb asks for absolute paths
// (pathModeAbs), so a relative one means the run fell back for an older
// golangci-lint that rejects --path-mode — and a relative path has no
// unambiguous base to resolve against (that is the whole reason for the flag).
// Judging it would mean guessing, and the cost of guessing wrong is suppressing
// a real finding, so the check simply declines to run. An empty filename is
// covered by the same rule.
//
// A path counts as missing ONLY when the stat fails with "does not exist".
// Anything present in any form counts as present — including a directory — and
// so does a path that could not be checked at all, such as one under a
// permission-denied parent. A check that cannot see a file must never claim the
// file is gone.
func pathPresent(p string) bool {
	if !filepath.IsAbs(p) {
		return true
	}
	_, err := statFile(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// lookPath is the PATH lookup seam (tests substitute it).
var lookPath = exec.LookPath

// LookBinary resolves the golangci-lint executable: PATH first, then the Go
// tool bin directory ($GOBIN, else $GOPATH/bin, else ~/go/bin).
//
// The fallback matters because the daemon does NOT run with the user's
// interactive PATH: it inherits the environment of whichever `plumb serve`
// proxy spawned it, which is captured when that agent session starts and
// routinely lacks ~/go/bin. `go install`-ed tools land exactly there, so PATH
// alone silently disables this analyser on a perfectly well set-up machine.
//
// Exported so `plumb doctor` reports the same binary the analyser will actually
// run — a doctor check that resolved differently would be worse than none.
func LookBinary() (string, bool) {
	if bin, err := lookPath("golangci-lint"); err == nil {
		return bin, true
	}
	for _, dir := range goToolBinDirs() {
		candidate := filepath.Join(dir, "golangci-lint")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

// goToolBinDirs lists the directories `go install` writes to, most specific
// first. Read from the environment rather than shelling out to `go env`, which
// would spawn a process on a write path that must stay cheap.
func goToolBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		// GOPATH may be a list; only the first element receives installs.
		first := strings.Split(gopath, string(os.PathListSeparator))[0]
		if first != "" {
			dirs = append(dirs, filepath.Join(first, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// unavailableOnce bounds the "not found" log to one line per daemon lifetime:
// the analyser runs on every Go write, so an unconditional warning would flood
// the log, and that is precisely how a warning ends up being ignored.
var unavailableOnce sync.Once

func logUnavailableOnce(ctx context.Context) {
	unavailableOnce.Do(func() {
		slog.InfoContext(ctx, "quality: golangci-lint not found — post-write Go quality findings are disabled",
			"searched", append([]string{"PATH"}, goToolBinDirs()...),
			"hint", "install golangci-lint, or put its directory on the PATH the daemon inherits")
	})
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

// lintOutput is the top-level JSON structure emitted by golangci-lint's JSON output.
type lintOutput struct {
	Issues []lintIssue `json:"Issues"`
}

type lintIssue struct {
	FromLinter  string   `json:"FromLinter"`
	Text        string   `json:"Text"`
	Severity    string   `json:"Severity"`
	SourceLines []string `json:"SourceLines"`
	Pos         struct {
		Filename string `json:"Filename"`
		Line     int    `json:"Line"`
		Column   int    `json:"Column"`
	} `json:"Pos"`
}

// parseOutput converts golangci-lint's JSON document into findings. Malformed
// output yields no findings rather than an error: the write has already
// succeeded and a linter we cannot parse must not fail it.
func parseOutput(data []byte, source string) []quality.Finding {
	var out lintOutput
	// golangci-lint appends a human-readable summary after the JSON document on
	// stdout when issues are present, so decode only the leading JSON value and
	// ignore any trailing text (json.Unmarshal would reject it).
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&out); err != nil {
		return nil // malformed output — treat as no findings
	}
	findings := make([]quality.Finding, 0, len(out.Issues))
	for _, issue := range out.Issues {
		sev := quality.SeverityWarning
		if issue.Severity == "error" {
			sev = quality.SeverityError
		}
		findings = append(findings, quality.Finding{
			File:     issue.Pos.Filename,
			Line:     issue.Pos.Line,
			Column:   issue.Pos.Column,
			Severity: sev,
			Code:     issue.FromLinter,
			Message:  issue.Text,
			Source:   source,
		})
	}
	return findings
}
