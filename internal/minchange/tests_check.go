package minchange

import (
	"fmt"
	"sort"
	"strings"
)

// codeExtensions are the source-file suffixes whose logic changes warrant a
// colocated test change. Data/markup/config files are excluded — a change to
// them is not "logic" in the sense the check cares about.
var codeExtensions = map[string]bool{
	".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true,
	".jsx": true, ".rs": true, ".java": true, ".kt": true, ".rb": true,
	".c": true, ".cc": true, ".cpp": true, ".h": true, ".hpp": true,
	".cs": true, ".swift": true, ".zig": true, ".php": true, ".scala": true,
}

// verificationGapFindings emits a single aggregate finding when the diff changes
// source logic but touches no test file at all. It is proven from the diff's
// file set (High confidence) and is the most actionable check, so it is a
// Warning. When at least one test file changed, the check stays silent — it does
// not attempt to judge whether the right tests changed.
func verificationGapFindings(diff *Diff, opts Options) []Finding {
	var sourceFiles []string
	testChanged := false
	for i := range diff.Files {
		f := &diff.Files[i]
		if f.IsBinary {
			continue
		}
		if isTestFile(f.Path) {
			if hasContentChange(f) {
				testChanged = true
			}
			continue
		}
		if isCodeFile(f.Path) && hasContentChange(f) {
			sourceFiles = append(sourceFiles, f.Path)
		}
	}
	if testChanged || len(sourceFiles) == 0 {
		return nil
	}
	sort.Strings(sourceFiles)

	f := Finding{
		Severity:   Warning,
		Kind:       KindVerificationGap,
		Confidence: High,
		Rationale:  "source logic changed but no test file was added or modified in this diff — the change is unverified",
		Evidence:   "changed source without a colocated test change: " + strings.Join(cap10(sourceFiles), ", "),
	}
	if opts.IncludeSuggestions {
		f.Alternative = fmt.Sprintf(
			"identify what to run with topology_affected({\"files\": [%s]}), then run those tests with run_task({\"slot\": \"test\"}) — add or extend a test before relying on the change",
			quoteJoin(cap10(sourceFiles)))
	}
	return []Finding{f}
}

// isTestFile reports whether path is a test/spec file across the common
// conventions (Go _test.go, JS/TS .test./.spec., Python test_*, Rust tests/…).
func isTestFile(path string) bool {
	base := pathBase(path)
	switch {
	case isGoTestFile(path):
		return true
	case strings.Contains(base, ".test.") || strings.Contains(base, ".spec."):
		return true
	case strings.HasPrefix(base, "test_") || strings.HasSuffix(strings.TrimSuffix(base, extOf(base)), "_test"):
		return true
	case strings.Contains(path, "/tests/") || strings.HasPrefix(path, "tests/"):
		return true
	default:
		return false
	}
}

// isCodeFile reports whether path is a source file whose logic changes warrant a
// test.
func isCodeFile(path string) bool { return codeExtensions[extOf(pathBase(path))] }

// hasContentChange reports whether the file diff has any added or removed line —
// i.e. a real content change rather than a pure rename/mode change.
func hasContentChange(f *FileDiff) bool {
	for h := range f.Hunks {
		for _, ln := range f.Hunks[h].Lines {
			if ln.Kind == Added || ln.Kind == Removed {
				return true
			}
		}
	}
	return false
}

// extOf returns the lowercase extension of a base name, including the dot.
func extOf(base string) string {
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		return strings.ToLower(base[i:])
	}
	return ""
}

// cap10 caps a slice at ten entries, appending an ellipsis marker when longer,
// so the evidence line stays bounded regardless of diff size.
func cap10(s []string) []string {
	const limit = 10
	if len(s) <= limit {
		return s
	}
	out := append([]string{}, s[:limit]...)
	return append(out, fmt.Sprintf("… (+%d more)", len(s)-limit))
}

// quoteJoin renders paths as a JSON string array body ("a", "b"), skipping the
// ellipsis marker cap10 may have appended.
func quoteJoin(paths []string) string {
	var q []string
	for _, p := range paths {
		if strings.HasPrefix(p, "…") {
			continue
		}
		q = append(q, `"`+p+`"`)
	}
	return strings.Join(q, ", ")
}
