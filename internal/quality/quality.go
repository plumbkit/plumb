// Package quality runs offline code analysers (golangci-lint, ruff, …) against
// changed files and returns structured findings. Write tools append findings to
// their response so agents can self-correct style regressions before the next
// turn — without waiting for CI.
//
// The package is language-agnostic: each Analyser reports which file extensions
// it supports and handles its own subprocess invocation. A nil or missing
// analyser silently produces no findings.
//
// Concurrency: Runner is safe for concurrent use after Start returns.
package quality

import "context"

// Severity classifies a finding's urgency.
type Severity int

const (
	SeverityInfo    Severity = iota
	SeverityWarning          // style, maintainability
	SeverityError            // correctness, security
)

func (s Severity) String() string {
	switch s {
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return "info"
	}
}

// Finding is one diagnostic produced by an Analyser.
type Finding struct {
	File     string // workspace-relative or absolute path
	Line     int    // 1-based; 0 if unknown
	Column   int    // 1-based; 0 if unknown
	Severity Severity
	Code     string // linter rule name, e.g. "ineffassign"
	Message  string
	Source   string // analyser name, e.g. "golangci-lint"
}

// Analyser runs an offline quality tool against a set of files.
type Analyser interface {
	// Name returns the analyser's canonical name (e.g. "golangci-lint").
	Name() string
	// Supports reports whether the analyser can process path based on its
	// file extension and surrounding project markers.
	Supports(path string) bool
	// Analyse runs the analyser against files and returns any findings.
	// A missing binary or unconfigured tool must return (nil, nil) — not
	// an error — so callers degrade gracefully.
	Analyse(ctx context.Context, files []string) ([]Finding, error)
}
