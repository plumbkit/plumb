package tools

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// context_scope.go — the scope filter context_for_task applies to every
// candidate it considers. A1 uses it on seeds; the expansion chunk applies it
// again on every hop's frontier, so there is one place that decides whether a
// path may appear in a pack.
//
// The filter only ever narrows. within and corpora can remove candidates that
// the agent's root would otherwise admit, and nothing here can admit a path the
// root does not contain.

// Corpus names. A candidate belongs to exactly one.
const (
	corpusCode   = "code"
	corpusDocs   = "docs"
	corpusMemory = "memory"
)

// memoryDirPrefix is the workspace-relative directory that holds project
// memories. A path under it is memory-corpus whatever its extension.
const memoryDirPrefix = ".plumb/memories/"

// isDocsPath reports whether rel names a prose document rather than code. It is
// extension-based on purpose: a directory called docs/ may hold Go examples,
// and those are code.
func isDocsPath(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".md", ".markdown", ".mdx", ".rst", ".adoc":
		return true
	}
	return false
}

// corpusOfPath classifies a workspace-relative, slash-separated path.
func corpusOfPath(rel string) string {
	switch {
	case strings.HasPrefix(rel, memoryDirPrefix):
		return corpusMemory
	case isDocsPath(rel):
		return corpusDocs
	default:
		return corpusCode
	}
}

// contextScope is the narrowing policy for one call. The zero value of within
// and corpora admits everything under root.
//
// Concurrency: immutable after newContextScope returns, so safe for concurrent
// use.
type contextScope struct {
	root    string          // canonical root of the calling agent
	within  []string        // normalised, root-relative patterns; empty means no narrowing
	corpora map[string]bool // allowlist; empty means every corpus
}

// newContextScope builds the scope for root. within entries are root-relative
// (or absolute paths inside root) literals or globs; an entry outside root, or a
// malformed glob, is a refused argument rather than a silently ignored one,
// because a dropped narrowing entry would widen the pack.
func newContextScope(root string, within, corpora []string) (contextScope, error) {
	s := contextScope{root: root}
	for _, w := range within {
		norm, err := normaliseWithin(root, w)
		if err != nil {
			return contextScope{}, err
		}
		s.within = append(s.within, norm)
	}
	if len(corpora) > 0 {
		s.corpora = make(map[string]bool, len(corpora))
		for _, c := range corpora {
			s.corpora[c] = true
		}
	}
	return s, nil
}

// normaliseWithin turns one within entry into a clean root-relative pattern.
func normaliseWithin(root, entry string) (string, error) {
	e := strings.TrimSpace(entry)
	if e == "" {
		return "", badArgument(errors.New("context_for_task: within contains an empty entry"))
	}
	if filepath.IsAbs(e) {
		rel := relWithinRoot(root, e)
		if rel == "" {
			return "", badArgument(fmt.Errorf("context_for_task: within entry %q is outside the workspace root; within only narrows", entry))
		}
		e = rel
	}
	e = path.Clean(filepath.ToSlash(e))
	if e == ".." || strings.HasPrefix(e, "../") {
		return "", badArgument(fmt.Errorf("context_for_task: within entry %q reaches outside the workspace root; within only narrows", entry))
	}
	if _, err := path.Match(e, ""); err != nil {
		return "", badArgument(fmt.Errorf("context_for_task: within entry %q is not a valid glob: %w", entry, err))
	}
	return e, nil
}

// allows reports whether candidate, in the given corpus, may appear in the
// pack. candidate is a root-relative slash path (as the index stores it) or an
// absolute path. When it may not, the second result is the reason, phrased so
// it can be shown to the agent without revealing anything about the excluded
// path itself.
func (s contextScope) allows(candidate, corpus string) (bool, string) {
	rel := s.relative(candidate)
	if rel == "" {
		return false, "outside the workspace root"
	}
	if len(s.corpora) > 0 && !s.corpora[corpus] {
		return false, fmt.Sprintf("corpus %s is not in corpora", corpus)
	}
	if len(s.within) == 0 {
		return true, ""
	}
	for _, w := range s.within {
		if withinMatches(w, rel) {
			return true, ""
		}
	}
	return false, "outside the within filter"
}

// relative maps candidate to a clean root-relative slash path, or "" when it
// does not lie inside root. A relative candidate is judged lexically (an index
// path has no symlinks to follow); an absolute one is canonicalised first.
func (s contextScope) relative(candidate string) string {
	if filepath.IsAbs(candidate) {
		return relWithinRoot(s.root, candidate)
	}
	clean := path.Clean(filepath.ToSlash(candidate))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return ""
	}
	return clean
}

// relWithinRoot returns abs as a root-relative slash path when it lies inside
// root after canonicalisation, and "" otherwise (including for root itself,
// which names no file). An absolute path carrying an unresolved ".." is never
// inside any root, matching PathWithinWorkspace.
func relWithinRoot(root, abs string) string {
	if hasParentTraversal(abs) {
		return ""
	}
	canon := canonicalRoot(abs)
	if !withinRoot(root, canon) {
		return ""
	}
	rel, err := filepath.Rel(root, canon)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// withinMatches reports whether rel is selected by one normalised within
// pattern. The grammar is deliberately small:
//
//   - A literal names a path from the root and selects it and everything under
//     it ("internal/tools" selects internal/tools/x.go).
//   - A glob containing "/" is anchored at the root and selects any path whose
//     leading directories match, so "internal/*" selects internal/tools/x.go.
//   - A glob without "/" matches a single path segment anywhere, so "*.go"
//     selects pkg/x.go (gitignore-style).
//   - A leading "**/" makes a pattern match from any depth; a trailing "/**"
//     is redundant, since a pattern already selects what lies beneath it.
func withinMatches(pattern, rel string) bool {
	pat := strings.TrimSuffix(pattern, "/**")
	anywhere := false
	if p, ok := strings.CutPrefix(pat, "**/"); ok {
		pat, anywhere = p, true
	}
	if pat == "" || pat == "." {
		return true
	}
	if !strings.Contains(pat, "/") && strings.ContainsAny(pat, "*?[") {
		anywhere = true
	}
	segs := strings.Split(rel, "/")
	lastStart := 0
	if anywhere {
		lastStart = len(segs) - 1
	}
	for start := 0; start <= lastStart; start++ {
		for end := start + 1; end <= len(segs); end++ {
			if patternMatches(pat, strings.Join(segs[start:end], "/")) {
				return true
			}
		}
	}
	return false
}

// patternMatches matches one pattern against one candidate sub-path.
func patternMatches(pat, cand string) bool {
	if !strings.ContainsAny(pat, "*?[") {
		return pat == cand
	}
	ok, err := path.Match(pat, cand)
	return err == nil && ok
}
