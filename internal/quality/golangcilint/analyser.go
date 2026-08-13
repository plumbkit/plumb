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

	// --output.json.path=stdout is the golangci-lint v2 spelling; the v1
	// --out-format=json flag was removed and errors with "unknown flag".
	args := append([]string{"run", "--output.json.path=stdout"}, files...)
	cmd := exec.CommandContext(ctx, bin, args...)
	// golangci-lint resolves the Go module from its working directory. The daemon
	// runs from "/", which is in no module, so anchor the run at the analysed file's
	// directory. files are the absolute paths of just-written source files.
	var base string
	if dir := filepath.Dir(files[0]); filepath.IsAbs(dir) {
		cmd.Dir = dir
		base = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// golangci-lint exits non-zero when it finds issues; that is not an error
	// for us — we parse the JSON output regardless of exit code.
	runErr := cmd.Run()

	// A successful run always writes a JSON document to stdout (even with zero
	// issues), so empty stdout means the linter failed to run rather than a
	// clean file. Surface that instead of silently reporting "no findings".
	if stdout.Len() == 0 {
		if runErr != nil {
			slog.WarnContext(ctx, "golangci-lint failed to run",
				"error", runErr, "stderr", stderrTail(stderr.String()))
		}
		return nil, nil
	}
	return flagStaleCache(parseOutput(stdout.Bytes(), a.Name()), base, a.Name()), nil
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
// base is the directory golangci-lint ran in (cmd.Dir), which is what its output
// paths are relative to; empty base means the process working directory, which is
// already what os.Stat resolves a relative path against.
func flagStaleCache(findings []quality.Finding, base, source string) []quality.Finding {
	if len(findings) == 0 || !allPathsMissing(findings, base) {
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
// at one os.Stat per DISTINCT path.
//
// Conservative by construction: a path counts as missing only when the stat
// fails with "does not exist". Anything present in any form counts as present —
// including a directory — and so does a path we could not check at all (an empty
// filename, or a stat that failed for another reason such as a permission-denied
// parent). A check that cannot see a file must never claim the file is gone.
func allPathsMissing(findings []quality.Finding, base string) bool {
	checked := make(map[string]bool, len(findings))
	for _, f := range findings {
		p := resolveFindingPath(f.File, base)
		present, seen := checked[p]
		if !seen {
			present = pathPresent(p)
			checked[p] = present
		}
		if present {
			return false
		}
	}
	return true
}

// resolveFindingPath maps a golangci-lint output path to one os.Stat can use.
// Measured behaviour: golangci-lint emits paths relative to its own working
// directory — running it at a module root reports "sub/deep/bad.go", one level
// down "deep/bad.go", and in the file's own directory "bad.go". Resolving
// against anything else (the module root, say) would make every finding stat as
// missing, which is the one way this check could turn real findings into a bogus
// stale-cache report.
func resolveFindingPath(p, base string) string {
	if p == "" || base == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func pathPresent(p string) bool {
	if p == "" {
		return true // nothing to check — cannot claim it is gone
	}
	_, err := os.Stat(p)
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
