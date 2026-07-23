package tools

import (
	"fmt"
	"strings"
)

// checkSamePackage refuses a Go move whose source and destination declare
// different packages (the same-directory _test-package case), which would change
// reference semantics v1 does not rewrite. A no-op for non-Go files or when
// either package clause is absent.
func checkSamePackage(srcPath, dstPath string, srcBefore, destBefore []byte) error {
	if !isGoFile(srcPath) || !isGoFile(dstPath) {
		return nil
	}
	srcPkg := goPackageClause(srcBefore)
	dstPkg := goPackageClause(destBefore)
	if srcPkg != "" && dstPkg != "" && srcPkg != dstPkg {
		return fmt.Errorf("move_symbol: cross-package move not supported in v1 — source is %q, destination is %q. Moving between packages changes reference/import semantics that v1 does not rewrite", srcPkg, dstPkg)
	}
	return nil
}

// goPackageClause returns the trimmed `package X` line of a Go source file, or
// "" when none is present.
func goPackageClause(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if tl := strings.TrimSpace(line); strings.HasPrefix(tl, "package ") {
			return tl
		}
	}
	return ""
}

func isGoFile(path string) bool { return strings.HasSuffix(path, ".go") }

// checkGoBuildTags refuses a Go move whose source and destination carry
// different //go:build (or legacy `// +build`) constraints. Two same-directory
// Go files are allowed to compile under different platform/tag constraints;
// moving a declaration between them without reconciling those constraints would
// silently change what compiles where. Identical constraint sets — including
// both files having none — proceed. A no-op for non-Go files (same honesty
// pattern as checkSamePackage).
func checkGoBuildTags(srcPath, dstPath string, srcBefore, destBefore []byte) error {
	if !isGoFile(srcPath) || !isGoFile(dstPath) {
		return nil
	}
	srcTags := goBuildConstraints(srcBefore)
	dstTags := goBuildConstraints(destBefore)
	if strings.Join(srcTags, "\n") == strings.Join(dstTags, "\n") {
		return nil
	}
	return fmt.Errorf("move_symbol: source and destination have different Go build constraints — source: %s, destination: %s. Moving a declaration between files under different build tags would silently change what compiles per platform/tag, which v1 does not reconcile; align the constraints or move by hand",
		describeBuildTags(srcTags), describeBuildTags(dstTags))
}

func describeBuildTags(tags []string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	return strings.Join(tags, "; ")
}

// goBuildConstraints returns the trimmed //go:build and legacy `// +build`
// lines found in a Go file's leading comment block — the region before the
// package clause, which is where build constraints are required to live.
func goBuildConstraints(b []byte) []string {
	var tags []string
	for _, line := range strings.Split(string(b), "\n") {
		tl := strings.TrimSpace(line)
		if strings.HasPrefix(tl, "package ") {
			break
		}
		if strings.HasPrefix(tl, "//go:build") || isLegacyBuildTagLine(tl) {
			tags = append(tags, tl)
		}
	}
	return tags
}

// isLegacyBuildTagLine reports whether line is a legacy `// +build ...`
// constraint comment (the pre-Go-1.17 form; still honoured alongside
// //go:build).
func isLegacyBuildTagLine(line string) bool {
	rest, ok := strings.CutPrefix(line, "//")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(rest), "+build")
}

// normalizeRemovalSeam cleans up the seam left behind by deleting a declaration
// at byte offset seam in after (the already-edited source content): a run of 3+
// consecutive newlines spanning the seam collapses to exactly two (one blank
// line, matching normal decl-to-decl spacing), and — when the removed
// declaration was the last thing in the file — the result is trimmed to a
// single trailing newline (or none, for a now-empty file). This is
// language-neutral text normalisation at the seam only; nothing else in the
// file is reflowed.
func normalizeRemovalSeam(after []byte, seam int) []byte {
	if seam < 0 || seam > len(after) {
		return after
	}
	i := seam
	for i > 0 && after[i-1] == '\n' {
		i--
	}
	j := seam
	for j < len(after) && after[j] == '\n' {
		j++
	}
	if i == seam && j == seam {
		return after // no newlines border the seam; nothing to normalise
	}
	prefix, suffix := after[:i], after[j:]
	if len(suffix) == 0 {
		// The removed declaration was the last thing in the file.
		if len(prefix) == 0 {
			return prefix
		}
		out := make([]byte, len(prefix)+1)
		copy(out, prefix)
		out[len(prefix)] = '\n'
		return out
	}
	nlCount := j - i
	if nlCount > 2 {
		nlCount = 2
	}
	out := make([]byte, 0, len(prefix)+nlCount+len(suffix))
	out = append(out, prefix...)
	for k := 0; k < nlCount; k++ {
		out = append(out, '\n')
	}
	out = append(out, suffix...)
	return out
}
