package tools

import "strings"

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
