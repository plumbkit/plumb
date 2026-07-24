package minchange

import (
	"fmt"
	"strings"
)

// stdlibEquivalent maps a dependency (by module path or npm package name) to a
// well-known standard-library alternative. The list is deliberately tiny and
// conservative: every entry is a dependency whose common use is genuinely
// covered by the stdlib of its language, so the resulting Info finding is
// defensible rather than opinionated. It is NOT a claim the dependency is
// unjustified — only a prompt to consider the stdlib path.
var stdlibEquivalent = map[string]string{
	// Go
	"github.com/pkg/errors":      "the standard library's error wrapping — fmt.Errorf(\"…: %w\", err) with errors.Is/errors.As (Go 1.13+)",
	"github.com/sirupsen/logrus": "log/slog, the standard structured logger (Go 1.21+)",
	"go.uber.org/zap":            "log/slog, the standard structured logger (Go 1.21+)",
	"github.com/golang/protobuf": "google.golang.org/protobuf (the current module); the old one is deprecated",
	"github.com/ghodss/yaml":     "a maintained YAML module; ghodss/yaml is archived",
	// npm
	"left-pad":  "String.prototype.padStart",
	"is-odd":    "a one-line `n % 2 !== 0` check",
	"is-even":   "a one-line `n % 2 === 0` check",
	"is-number": "typeof x === 'number' (and Number.isFinite for validation)",
	"is-array":  "Array.isArray",
}

// dependencyFindings flags a dependency newly added to go.mod or package.json
// when it is on the curated stdlib-equivalent list. Info-level only, and never
// stronger: it does not analyse how the dependency is used, so it can only
// suggest, never assert.
func dependencyFindings(diff *Diff, opts Options) []Finding {
	var out []Finding
	for i := range diff.Files {
		f := &diff.Files[i]
		if f.IsBinary || f.IsDelete {
			continue
		}
		base := pathBase(f.Path)
		switch base {
		case "go.mod":
			out = append(out, moduleFindings(f, goModAddedModules(f), opts)...)
		case "package.json":
			out = append(out, moduleFindings(f, packageJSONAddedDeps(f), opts)...)
		}
	}
	return out
}

// depAdd is a single newly-required dependency and the line that added it.
type depAdd struct {
	name string
	line int
	text string
}

// moduleFindings turns each added dependency that has a stdlib equivalent into a
// finding.
func moduleFindings(f *FileDiff, adds []depAdd, opts Options) []Finding {
	var out []Finding
	for _, a := range adds {
		alt, ok := stdlibEquivalent[a.name]
		if !ok {
			continue
		}
		fnd := Finding{
			Severity:   Info,
			Kind:       KindStdlibCandidate,
			Confidence: Low,
			File:       f.Path,
			Line:       a.line,
			Rationale:  fmt.Sprintf("%s was added as a dependency, but its common use is covered by the standard library", a.name),
			Evidence:   fmt.Sprintf("added: %s", strings.TrimSpace(a.text)),
		}
		if opts.IncludeSuggestions {
			fnd.Alternative = fmt.Sprintf("consider %s (keep the dependency if you rely on features the stdlib lacks)", alt)
		}
		out = append(out, fnd)
	}
	return out
}

// goModAddedModules returns the modules introduced by added (+) require lines in
// a go.mod diff. A require line removed elsewhere in the same diff (a version
// bump) is ignored, so only genuinely new modules are reported.
func goModAddedModules(f *FileDiff) []depAdd {
	removed := removedModulePaths(f)
	var out []depAdd
	for h := range f.Hunks {
		for _, ln := range f.Hunks[h].Lines {
			if ln.Kind != Added {
				continue
			}
			// An "// indirect" require was added by go mod tidy, not chosen by
			// the author — not a deliberate dependency decision to review.
			if strings.Contains(ln.Text, "// indirect") {
				continue
			}
			mod := requireModulePath(ln.Text)
			if mod == "" || removed[mod] {
				continue
			}
			out = append(out, depAdd{name: mod, line: ln.NewLineNo, text: ln.Text})
		}
	}
	return out
}

// removedModulePaths collects modules on removed (-) lines, so a version bump
// (remove old require, add new require of the same module) is not reported as a
// new dependency.
func removedModulePaths(f *FileDiff) map[string]bool {
	out := map[string]bool{}
	for h := range f.Hunks {
		for _, ln := range f.Hunks[h].Lines {
			if ln.Kind != Removed {
				continue
			}
			if mod := requireModulePath(ln.Text); mod != "" {
				out[mod] = true
			}
		}
	}
	return out
}

// requireModulePath extracts a module path from a go.mod require line, whether
// inside a require ( … ) block ("\tmod v1.2.3") or a single-line require
// ("require mod v1.2.3"). Returns "" for non-require lines and for the module's
// own `module` directive. `// indirect` markers are tolerated.
func requireModulePath(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "require ")
	t = strings.TrimSpace(t)
	fields := strings.Fields(t)
	if len(fields) < 2 {
		return ""
	}
	mod, ver := fields[0], fields[1]
	// A dependency line is "<module-path> v<semver>"; anything else (directives,
	// replace targets) is skipped.
	if !strings.HasPrefix(ver, "v") || !strings.Contains(mod, "/") {
		return ""
	}
	return mod
}

// packageJSONAddedDeps returns npm packages introduced by added lines within a
// package.json diff's dependency maps. It is line-based (not a full JSON parse,
// since a diff shows only fragments): a `"name": "version"` entry on an added
// line, excluding lines also removed (a version bump).
func packageJSONAddedDeps(f *FileDiff) []depAdd {
	removed := removedJSONKeys(f)
	var out []depAdd
	for h := range f.Hunks {
		for _, ln := range f.Hunks[h].Lines {
			if ln.Kind != Added {
				continue
			}
			name := jsonDepKey(ln.Text)
			if name == "" || removed[name] {
				continue
			}
			out = append(out, depAdd{name: name, line: ln.NewLineNo, text: ln.Text})
		}
	}
	return out
}

// removedJSONKeys collects dependency keys on removed lines (version bumps).
func removedJSONKeys(f *FileDiff) map[string]bool {
	out := map[string]bool{}
	for h := range f.Hunks {
		for _, ln := range f.Hunks[h].Lines {
			if ln.Kind != Removed {
				continue
			}
			if k := jsonDepKey(ln.Text); k != "" {
				out[k] = true
			}
		}
	}
	return out
}

// jsonDepKey extracts the package name from a `"name": "version"` package.json
// line, returning "" when the line is not a string-to-string entry (so section
// headers and braces are skipped). The value must look like a version spec to
// avoid matching arbitrary string fields.
func jsonDepKey(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimSuffix(t, ",")
	key, rest, ok := quotedString(t)
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, ":") {
		return ""
	}
	val, _, ok := quotedString(strings.TrimSpace(rest[1:]))
	if !ok || !looksLikeVersion(val) {
		return ""
	}
	return key
}

// quotedString reads a leading "…" token, returning its contents and the
// remaining text.
func quotedString(s string) (val, rest string, ok bool) {
	if len(s) == 0 || s[0] != '"' {
		return "", "", false
	}
	end := strings.IndexByte(s[1:], '"')
	if end < 0 {
		return "", "", false
	}
	return s[1 : 1+end], s[2+end:], true
}

// looksLikeVersion reports whether v resembles an npm version spec (a digit,
// or a range/tag prefix), so a dependency entry is distinguished from an
// arbitrary string field like "name" or "description".
func looksLikeVersion(v string) bool {
	if v == "" {
		return false
	}
	switch v[0] {
	case '^', '~', '>', '<', '=', '*':
		return true
	}
	return v[0] >= '0' && v[0] <= '9'
}

// pathBase returns the final path element of a slash-separated path.
func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
