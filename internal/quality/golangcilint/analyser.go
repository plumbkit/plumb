// Package golangcilint implements a quality.Analyser that shells out to
// golangci-lint. If golangci-lint is not on PATH the analyser silently returns
// no findings rather than erroring — the write still succeeds.
package golangcilint

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"

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
// Returns (nil, nil) if golangci-lint is not on PATH or exits with a
// "no issues" status.
func (a *Analyser) Analyse(ctx context.Context, files []string) ([]quality.Finding, error) {
	if len(files) == 0 {
		return nil, nil
	}
	bin, err := exec.LookPath("golangci-lint")
	if err != nil {
		return nil, nil // binary absent — silent skip
	}

	args := append([]string{"run", "--out-format=json"}, files...)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// golangci-lint exits non-zero when it finds issues; that is not an error
	// for us — we parse the JSON output regardless.
	_ = cmd.Run()

	if stdout.Len() == 0 {
		return nil, nil
	}
	return parseOutput(stdout.Bytes(), a.Name())
}

// lintOutput is the top-level JSON structure emitted by golangci-lint --out-format=json.
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
	if err := json.Unmarshal(data, &out); err != nil {
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
