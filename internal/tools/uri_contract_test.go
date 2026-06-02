package tools

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// uriSchemaProp matches a JSON-schema declaration of a `uri` string property in
// a tool's embedded InputSchema (e.g. `"uri": {`). It is how this test
// auto-discovers every uri-taking tool without a hand-maintained list.
var uriSchemaProp = regexp.MustCompile(`"uri"\s*:\s*\{`)

// TestURITools_NormalisePlainPaths is the contract guard for the 0.8.3 change
// that let every uri-taking tool accept a plain absolute path as readily as a
// file:// URI: any tool whose InputSchema declares a `uri` property MUST run the
// value through toFileURI at parse time, or a plain path reaches the language
// server as-is and the LSP rejects it.
//
// The check is intentionally same-file: plumb's convention is that value
// handling lives in the tool (the mcp alias layer is key-rename-only), so the
// normalisation call belongs in the same source file as the schema. If a future
// refactor centralises uri parsing, update this test to match the new seam.
func TestURITools_NormalisePlainPaths(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	var checked []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !uriSchemaProp.Match(src) {
			continue
		}
		if !strings.Contains(string(src), "toFileURI(") {
			t.Errorf("%s declares a \"uri\" schema property but never calls toFileURI — "+
				"a plain absolute path would reach the LSP unnormalised. Normalise it at parse time.", name)
		}
		checked = append(checked, name)
	}

	// Guard against the scan silently matching nothing (e.g. a glob/regex
	// regression): there are well over a dozen uri-taking tools.
	if len(checked) < 10 {
		t.Fatalf("expected the scan to cover the uri-taking tools, only matched %d: %v", len(checked), checked)
	}
}
