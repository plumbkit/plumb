package tools

import (
	"fmt"
	"path/filepath"
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
// different build constraints — either EXPLICIT (//go:build or legacy
// `// +build` comments) or IMPLICIT (the _GOOS, _GOARCH, _GOOS_GOARCH, and
// _test filename-suffix conventions Go's build system honours just as
// strictly — e.g. handlers_linux.go, or any_test.go). Two same-directory Go
// files are allowed to differ on either axis; moving a declaration between
// them without reconciling the constraints would silently change what
// compiles per platform/tag, or drop the code from the production build
// entirely (the _test.go case).
//
// The filename-derived and comment-derived constraint sets are compared
// INDEPENDENTLY, not for semantic equivalence: a plain foo.go carrying
// `//go:build linux` moved into a foo_linux.go is refused even though the two
// arguably express the same restriction, because proving that general
// equivalence (arbitrary build expressions vs. filename tags) is out of scope
// for v1. Refusing on any asymmetry is the conservative, honest choice — see
// TestMoveSymbol_RefusesCommentVsFilenameAsymmetry. Identical constraints on
// both axes — including both being entirely unconstrained — proceed. A no-op
// for non-Go files (same honesty pattern as checkSamePackage).
func checkGoBuildTags(srcPath, dstPath string, srcBefore, destBefore []byte) error {
	if !isGoFile(srcPath) || !isGoFile(dstPath) {
		return nil
	}
	srcFN := filenameConstraints(srcPath)
	dstFN := filenameConstraints(dstPath)
	srcTags := goBuildConstraints(srcBefore)
	dstTags := goBuildConstraints(destBefore)
	if srcFN == dstFN && strings.Join(srcTags, "\n") == strings.Join(dstTags, "\n") {
		return nil
	}
	return fmt.Errorf("move_symbol: source and destination have different Go build constraints — source is %s, destination is %s. Moving a declaration between files under different build constraints would silently change what compiles per platform/tag (or drop it from the build entirely, for _test.go), which v1 does not reconcile; align the constraints or move by hand",
		describeBuildConstraints(srcFN, srcTags), describeBuildConstraints(dstFN, dstTags))
}

// describeBuildConstraints renders one file's effective constraint set —
// filename-derived and comment-derived — for the checkGoBuildTags refusal
// message.
func describeBuildConstraints(fn filenameSig, tags []string) string {
	var parts []string
	if d := fn.describe(); d != "" {
		parts = append(parts, d)
	}
	if len(tags) > 0 {
		parts = append(parts, describeBuildTags(tags))
	}
	if len(parts) == 0 {
		return "unconstrained"
	}
	return strings.Join(parts, "; ")
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

// knownGOOS and knownGOARCH are the GOOS/GOARCH values Go's build system
// currently recognises for the filename-suffix convention (`go tool dist
// list`). Go occasionally adds new ports; extend these if move_symbol starts
// missing a valid suffix on a newer Go toolchain.
var knownGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "illumos": true, "ios": true, "js": true, "linux": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true,
	"wasip1": true, "windows": true,
}

var knownGOARCH = map[string]bool{
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mipsle": true, "mips64": true, "mips64le": true,
	"ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

func isGOOS(s string) bool   { return knownGOOS[s] }
func isGOARCH(s string) bool { return knownGOARCH[s] }

// filenameSig is the implicit build constraint a Go filename encodes per the
// stdlib's own convention: a trailing _GOOS, _GOARCH, or _GOOS_GOARCH
// component (only when it is not the whole base name — "linux.go" carries no
// constraint, "foo_linux.go" does), plus test-ness (a _test suffix). The zero
// value means "unconstrained" and compares equal (==) since every field is a
// plain comparable.
type filenameSig struct {
	goos   string
	goarch string
	test   bool
}

// filenameConstraints derives name's implicit build constraint from its base
// filename, following the same _GOOS / _GOARCH / _GOOS_GOARCH / _test
// suffix rules the go command itself applies (see `go help buildconstraint`).
func filenameConstraints(name string) filenameSig {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	parts := strings.Split(base, "_")
	var sig filenameSig
	if len(parts) > 1 && parts[len(parts)-1] == "test" {
		sig.test = true
		parts = parts[:len(parts)-1]
	}
	switch {
	case len(parts) > 2 && isGOOS(parts[len(parts)-2]) && isGOARCH(parts[len(parts)-1]):
		sig.goos = parts[len(parts)-2]
		sig.goarch = parts[len(parts)-1]
	case len(parts) > 1 && isGOOS(parts[len(parts)-1]):
		sig.goos = parts[len(parts)-1]
	case len(parts) > 1 && isGOARCH(parts[len(parts)-1]):
		sig.goarch = parts[len(parts)-1]
	}
	return sig
}

// describe renders sig for an error message, e.g. "linux-only (_linux.go)",
// "linux/amd64-only, test-only (_linux_amd64_test.go)", or "" when sig is
// unconstrained.
func (s filenameSig) describe() string {
	if s.goos == "" && s.goarch == "" && !s.test {
		return ""
	}
	var comps, labelParts []string
	switch {
	case s.goos != "" && s.goarch != "":
		comps = append(comps, s.goos, s.goarch)
		labelParts = append(labelParts, s.goos+"/"+s.goarch+"-only")
	case s.goos != "":
		comps = append(comps, s.goos)
		labelParts = append(labelParts, s.goos+"-only")
	case s.goarch != "":
		comps = append(comps, s.goarch)
		labelParts = append(labelParts, s.goarch+"-only")
	}
	if s.test {
		comps = append(comps, "test")
		labelParts = append(labelParts, "test-only")
	}
	return fmt.Sprintf("%s (_%s.go)", strings.Join(labelParts, ", "), strings.Join(comps, "_"))
}

// normalizeRemovalSeam cleans up the seam left behind by deleting a declaration
// at byte offset seam in after (the already-edited source content): a run of 3+
// consecutive newlines spanning the seam collapses to exactly two (one blank
// line, matching normal decl-to-decl spacing), and — when the removed
// declaration was the last thing in the file — the result is trimmed to a
// single trailing newline (or none, for a now-empty file). This is text
// normalisation at the seam only — LF-only (a CRLF file's \r\n line endings
// are left un-normalised; harmless, since the seam itself is unaffected).
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
