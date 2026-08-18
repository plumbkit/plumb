package tools

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// resolvePath resolves a path argument for filesystem tools. Strips a leading
// file:// scheme, then anchors a relative (or empty) path to the workspace root
// returned by ws. An absolute path is returned unchanged.
//
// When ws is nil or resolves to "" (an unattached session) a relative path is
// returned cleaned but still relative — deliberately NOT anchored to
// os.Getwd(). The daemon is a singleton whose working directory is unrelated to
// any workspace, so resolving against it would silently touch the wrong file;
// leaving the path relative lets the boundary check reject it honestly instead.
func resolvePath(ctx context.Context, path string, ws WorkspaceFn) string {
	p := paths.URIToPath(path)
	if filepath.IsAbs(p) {
		return p
	}
	base := ""
	if ws != nil {
		base = ws(ctx)
	}
	if base == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

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
//
// Windows-safe conversion (paths.PathToURI) applies only to an absolute path; a
// relative s keeps the bare "file://" + s form so it stays the server-rejected
// value callers rely on rather than gaining a spurious leading slash.
func toFileURI(s string) string {
	if s == "" || strings.HasPrefix(s, "file://") {
		return s
	}
	if !filepath.IsAbs(s) {
		return "file://" + s
	}
	return paths.PathToURI(s)
}

// toFileURIAnchored is toFileURI for uri-taking tools that should accept a
// workspace-relative path. A relative s is anchored to the workspace root
// returned by ws BEFORE the file:// scheme is added, so the language server and
// the routing proxy's pool.Detect never see a relative (and thus invalid) URI.
// An absolute path or existing file:// URI round-trips unchanged; when ws is
// nil or resolves to "" a relative s is left relative (cleaned), so the
// boundary check rejects it rather than producing a bogus file://app/... URI.
func toFileURIAnchored(ctx context.Context, s string, ws WorkspaceFn) string {
	if s == "" || strings.HasPrefix(s, "file://") {
		return s
	}
	if !filepath.IsAbs(s) {
		if ws != nil {
			if base := ws(ctx); base != "" {
				s = filepath.Join(base, s)
			}
		}
	}
	if !filepath.IsAbs(s) {
		return "file://" + s
	}
	return paths.PathToURI(s)
}
