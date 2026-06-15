package tools

import (
	"path/filepath"
	"strings"
)

// toFileURI normalises a filesystem path or file:// URI to a file:// URI, so
// every uri-taking tool can accept a plain absolute path as readily as a
// file:// URI. It strips any existing file:// prefix and re-adds it: a value
// already in file:// form round-trips unchanged, and an empty string stays
// empty.
//
// LSP queries require an absolute URI; a relative path produces a nominal
// file:// URI the server will reject, so relative paths remain unsupported on
// uri-taking tools. This generalises the long-standing normalisation in
// read_symbol's resolveReadSymbolPaths.
func toFileURI(s string) string {
	if s == "" || strings.HasPrefix(s, "file://") {
		return s
	}
	return "file://" + s
}

// toFileURIAnchored is toFileURI for uri-taking tools that should accept a
// workspace-relative path. A relative s is anchored to the workspace root
// returned by ws BEFORE the file:// scheme is added, so the language server and
// the routing proxy's pool.Detect never see a relative (and thus invalid) URI.
// An absolute path or existing file:// URI round-trips unchanged; when ws is
// nil or resolves to "" a relative s is left relative (cleaned), so the
// boundary check rejects it rather than producing a bogus file://app/... URI.
func toFileURIAnchored(s string, ws WorkspaceFn) string {
	if s == "" || strings.HasPrefix(s, "file://") {
		return s
	}
	if !filepath.IsAbs(s) {
		if ws != nil {
			if base := ws(); base != "" {
				s = filepath.Join(base, s)
			}
		}
	}
	return "file://" + s
}
