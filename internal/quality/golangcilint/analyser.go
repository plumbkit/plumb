// Package golangcilint implements a quality.Analyser that shells out to
// golangci-lint. If golangci-lint is not on PATH the analyser silently returns
// no findings rather than erroring — the write still succeeds.
package golangcilint

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"

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
	bin, err := exec.LookPath("golangci-lint")
	if err != nil {
		return nil, nil // binary absent — silent skip
	}

	// --output.json.path=stdout is the golangci-lint v2 spelling; the v1
	// --out-format=json flag was removed and errors with "unknown flag".
	args := append([]string{"run", "--output.json.path=stdout"}, files...)
	cmd := exec.CommandContext(ctx, bin, args...)
	// golangci-lint resolves the Go module from its working directory. The daemon
	// runs from "/", which is in no module, so anchor the run at the analysed file's
	// directory. files are the absolute paths of just-written source files.
	if dir := filepath.Dir(files[0]); filepath.IsAbs(dir) {
		cmd.Dir = dir
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
	return parseOutput(stdout.Bytes(), a.Name())
}

// stderrTail returns the trailing portion of stderr, bounded, for diagnostics.
func stderrTail(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = "…" + s[len(s)-max:]
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

func parseOutput(data []byte, source string) ([]quality.Finding, error) {
	var out lintOutput
	// golangci-lint appends a human-readable summary after the JSON document on
	// stdout when issues are present, so decode only the leading JSON value and
	// ignore any trailing text (json.Unmarshal would reject it).
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&out); err != nil {
		return nil, nil // malformed output — treat as no findings
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
	return findings, nil
}
