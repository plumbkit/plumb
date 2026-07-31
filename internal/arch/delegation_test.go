package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// delegationSite is one place a rule's Literal appears in production code.
type delegationSite struct {
	pkg       string // package path relative to the module root
	name      string // enclosing function, or the value-spec ident for a package-level literal
	pos       string // "file:line" of the first literal hit, for the failure message
	delegates bool   // the enclosing function also calls the rule's Delegate
	funcLevel bool   // false for a package-level const/var literal
}

// delegationSitesInFile returns every site in one parsed file where the
// rule's Literal appears as an exact string literal. It is a pure function of
// the parsed file so the red case — a hand-rolled copy — stays pinned by the
// checker self-tests below, which feed it synthetic source.
func delegationSitesInFile(fset *token.FileSet, pkg, filename string, file *ast.File, rule DelegationRule) []delegationSite {
	qualifier, imported := homeQualifier(file, rule.Home)
	funcName := rule.Delegate[strings.LastIndex(rule.Delegate, ".")+1:]

	var out []delegationSite
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			litPos, hits := literalHits(d.Body, rule.Literal)
			if hits == 0 {
				continue
			}
			out = append(out, delegationSite{
				pkg:       pkg,
				name:      d.Name.Name,
				pos:       delegationPos(fset, pkg, filename, litPos),
				delegates: imported && callsDelegate(d.Body, qualifier, funcName),
				funcLevel: true,
			})
		case *ast.GenDecl:
			// A package-level const or var carrying the literal is how a
			// hand-rolled copy would evade a body-only check; it is flagged
			// and can only be excused via the allowlist — there is no body to
			// delegate from.
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) == 0 {
					continue
				}
				for i, v := range vs.Values {
					litPos, hits := literalHits(v, rule.Literal)
					if hits == 0 {
						continue
					}
					// In a multi-name spec, attribute the hit to the name
					// paired with the matching value, not the first name.
					name := vs.Names[0].Name
					if len(vs.Names) == len(vs.Values) {
						name = vs.Names[i].Name
					}
					out = append(out, delegationSite{
						pkg:  pkg,
						name: name,
						pos:  delegationPos(fset, pkg, filename, litPos),
					})
				}
			}
		}
	}
	return out
}

// homeQualifier resolves how rule's Home package is referred to inside file:
// the local import name (honouring aliases; "." for a dot import), or
// ok=false when the file does not import Home at all — in which case no call
// in it can delegate.
func homeQualifier(file *ast.File, home string) (string, bool) {
	target := modulePath + "/" + home
	for _, spec := range file.Imports {
		if strings.Trim(spec.Path.Value, `"`) != target {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" {
				return "", false
			}
			return spec.Name.Name, true
		}
		return home[strings.LastIndex(home, "/")+1:], true
	}
	return "", false
}

// literalHits counts string literals in n that touch the resource: exactly
// equal to literal after unquoting, or a path ending in "/"+literal — the
// whole relative path spelled as one literal (".plumb/.gitignore") names the
// file just as surely as the bare name does. Never a bare substring match:
// prose mentions in error wraps and log messages must not match.
func literalHits(n ast.Node, literal string) (token.Pos, int) {
	var first token.Pos
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && (v == literal || strings.HasSuffix(v, "/"+literal)) {
			if count == 0 {
				first = lit.Pos()
			}
			count++
		}
		return true
	})
	return first, count
}

// callsDelegate reports whether body contains a call to qualifier.funcName,
// or a bare funcName call under a dot import (qualifier ".").
func callsDelegate(body *ast.BlockStmt, qualifier, funcName string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if qualifier == "." {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == funcName {
				found = true
			}
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == qualifier && sel.Sel.Name == funcName {
			found = true
		}
		return true
	})
	return found
}

func delegationPos(fset *token.FileSet, pkg, filename string, pos token.Pos) string {
	return filepath.Join(pkg, filepath.Base(filename)) + ":" + fsetLine(fset, pos)
}

// allDelegationSites walks internal/ and cmd/ and returns every site where
// the rule's Literal appears in production code outside the rule's Home.
//
// Test files are excluded, like allCallSites: a test that stages a .gitignore
// fixture is doing the forbidden thing on purpose. testdata directories hold
// fixture sources that are not part of the build.
func allDelegationSites(t *testing.T, root string, rule DelegationRule) []delegationSite {
	t.Helper()
	var out []delegationSite
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parsing %s: %v", path, perr)
			}
			rel, rerr := filepath.Rel(root, filepath.Dir(path))
			if rerr != nil {
				t.Fatalf("relativising %s: %v", path, rerr)
			}
			pkg := filepath.ToSlash(rel)
			if pkg == rule.Home {
				return nil
			}
			out = append(out, delegationSitesInFile(fset, pkg, path, file, rule)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}
	return out
}

// TestDelegationRules fails when a function touches a pinned resource's file
// directly instead of delegating to the single implementation.
//
// This is the rule that catches the next hand-rolled appender: it may share
// no name family with the original and open-code no pinned stdlib call, but
// it cannot avoid naming the file.
func TestDelegationRules(t *testing.T) {
	root := repoRoot(t)

	for _, rule := range DelegationRules {
		if _, ok := Layers[rule.Home]; !ok {
			t.Errorf("rule %q names Home %q, which is not a package in Layers",
				rule.Resource, rule.Home)
		}
		for _, s := range allDelegationSites(t, root, rule) {
			if s.funcLevel && s.delegates {
				continue
			}
			key := s.pkg + "." + s.name
			reason, allowed := rule.Allowed[key]
			switch {
			case !allowed:
				t.Errorf("%s: %s contains the literal %q without delegating to %s.\n    %s\n    If this site only reads the file (or is otherwise not an appender), add %q to the rule's Allowed map with the reason.",
					s.pos, s.name, rule.Literal, rule.Delegate, rule.Why, key)
			case strings.TrimSpace(reason) == "":
				t.Errorf("%s: allowlist entry %q has no reason recorded", s.pos, key)
			}
		}
	}
}

// TestDelegationAllowlistsAreLive is the companion to
// TestPinnedCallAllowlistsAreLive, in the deliberately strong form: an entry
// whose site no longer touches the literal without delegating — because the
// function was removed, renamed, or now delegates — must be deleted, since an
// exemption whose justification has stopped being true is worse than no rule.
func TestDelegationAllowlistsAreLive(t *testing.T) {
	root := repoRoot(t)

	for _, rule := range DelegationRules {
		needed := map[string]bool{}
		for _, s := range allDelegationSites(t, root, rule) {
			if !s.funcLevel || !s.delegates {
				needed[s.pkg+"."+s.name] = true
			}
		}
		for key := range rule.Allowed {
			if !needed[key] {
				t.Errorf("rule %q allowlists %q, but it no longer touches %q without delegating — remove the entry",
					rule.Resource, key, rule.Literal)
			}
		}
	}
}

// --- checker self-tests -----------------------------------------------------
//
// These pin the red case permanently: the tree walk above can only ever see
// the current (clean) tree, so the shape it exists to reject is encoded here
// as synthetic source fed straight to delegationSitesInFile.

// delegationHandRolledSrc is the body of internal/collab/gitignore.go as it
// stood at 1d1d415 (comments stripped — the parse discards them anyway): the
// last hand-rolled gitignore appender to be consolidated away, and the reason
// DelegationRules exists. It must always be flagged.
const delegationHandRolledSrc = `package collab

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ensureGitignore(dir string) error {
	const header = "# plumb cross-agent sharing (ephemeral; do not commit)"
	entries := []string{"collab.db", "collab.db-wal", "collab.db-shm"}

	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .gitignore: %w", err)
	}

	have := make(map[string]bool)
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}

	var missing []string
	for _, e := range entries {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	if !have[header] {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for _, e := range missing {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
`

// parseDelegationFixture parses synthetic source for the checker self-tests.
func parseDelegationFixture(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return fset, file
}

// gitignoreDelegationRule returns the rule under test.
func gitignoreDelegationRule(t *testing.T) DelegationRule {
	t.Helper()
	for _, r := range DelegationRules {
		if r.Literal == ".gitignore" {
			return r
		}
	}
	t.Fatal("no .gitignore DelegationRule declared")
	return DelegationRule{}
}

func TestDelegationChecker_FlagsHandRolledCopy(t *testing.T) {
	fset, file := parseDelegationFixture(t, delegationHandRolledSrc)
	sites := delegationSitesInFile(fset, "internal/collab", "gitignore.go", file, gitignoreDelegationRule(t))
	if len(sites) != 1 {
		t.Fatalf("want exactly one site, got %d: %+v", len(sites), sites)
	}
	s := sites[0]
	if s.name != "ensureGitignore" || s.delegates || !s.funcLevel {
		t.Errorf("hand-rolled copy not flagged as a non-delegating function: %+v", s)
	}
}

func TestDelegationChecker_AcceptsDelegatingWrapper(t *testing.T) {
	// Each wrapper names the file AND delegates; all import forms must be
	// recognised, because a miss here false-positives on legitimate code.
	fixtures := map[string]string{
		"plain": `package demo

import "github.com/plumbkit/plumb/internal/paths"

func ensureGitignore(dir string) error {
	_ = ".gitignore"
	return paths.EnsureGitignoreEntries(dir, "# hdr", []string{"demo.db"})
}
`,
		"aliased": `package demo

import p "github.com/plumbkit/plumb/internal/paths"

func ensureGitignore(dir string) error {
	_ = ".gitignore"
	return p.EnsureGitignoreEntries(dir, "# hdr", []string{"demo.db"})
}
`,
		"dot": `package demo

import . "github.com/plumbkit/plumb/internal/paths"

func ensureGitignore(dir string) error {
	_ = ".gitignore"
	return EnsureGitignoreEntries(dir, "# hdr", []string{"demo.db"})
}
`,
	}
	for name, src := range fixtures {
		t.Run(name, func(t *testing.T) {
			fset, file := parseDelegationFixture(t, src)
			sites := delegationSitesInFile(fset, "internal/demo", "gitignore.go", file, gitignoreDelegationRule(t))
			if len(sites) != 1 {
				t.Fatalf("want exactly one site, got %d: %+v", len(sites), sites)
			}
			if !sites[0].delegates {
				t.Errorf("delegating wrapper flagged as a violation: %+v", sites[0])
			}
		})
	}

	// The realistic post-migration wrapper passes a directory and never names
	// the file at all — zero sites, nothing to excuse.
	noLiteral := `package demo

import "github.com/plumbkit/plumb/internal/paths"

func ensureGitignore(dir string) error {
	return paths.EnsureGitignoreEntries(dir, "# hdr", []string{"demo.db"})
}
`
	fset, file := parseDelegationFixture(t, noLiteral)
	if sites := delegationSitesInFile(fset, "internal/demo", "gitignore.go", file, gitignoreDelegationRule(t)); len(sites) != 0 {
		t.Errorf("wrapper without the literal should yield no sites, got %+v", sites)
	}
}

func TestDelegationChecker_FlagsCombinedPathLiteral(t *testing.T) {
	// A copy that spells the whole relative path as one literal contains no
	// bare ".gitignore" literal; the path-suffix match catches it, because a
	// single-literal filepath.Join argument is a shape that genuinely occurs.
	src := `package demo

import (
	"os"
	"path/filepath"
)

func writeIgnore(ws string) error {
	return os.WriteFile(filepath.Join(ws, ".plumb/.gitignore"), []byte("x\n"), 0o644)
}
`
	fset, file := parseDelegationFixture(t, src)
	sites := delegationSitesInFile(fset, "internal/demo", "ignore.go", file, gitignoreDelegationRule(t))
	if len(sites) != 1 {
		t.Fatalf("want exactly one site, got %d: %+v", len(sites), sites)
	}
	if s := sites[0]; s.name != "writeIgnore" || s.delegates || !s.funcLevel {
		t.Errorf("combined-path literal not flagged as expected: %+v", s)
	}
}

func TestDelegationChecker_FlagsPackageLevelLiteral(t *testing.T) {
	src := `package demo

const gitignoreName = ".gitignore"
`
	fset, file := parseDelegationFixture(t, src)
	sites := delegationSitesInFile(fset, "internal/demo", "names.go", file, gitignoreDelegationRule(t))
	if len(sites) != 1 {
		t.Fatalf("want exactly one site, got %d: %+v", len(sites), sites)
	}
	s := sites[0]
	if s.name != "gitignoreName" || s.funcLevel || s.delegates {
		t.Errorf("package-level literal not flagged as expected: %+v", s)
	}

	// In a multi-name spec the hit is attributed to the name paired with the
	// matching value, so the failure message names the right identifier.
	multi := `package demo

var other, ignoreName = "x", ".gitignore"
`
	fset, file = parseDelegationFixture(t, multi)
	sites = delegationSitesInFile(fset, "internal/demo", "names.go", file, gitignoreDelegationRule(t))
	if len(sites) != 1 {
		t.Fatalf("want exactly one site for the multi-name spec, got %d: %+v", len(sites), sites)
	}
	if s := sites[0]; s.name != "ignoreName" || s.funcLevel || s.delegates {
		t.Errorf("multi-name spec misattributed: %+v", s)
	}
}

func TestDelegationChecker_IgnoresProseLiterals(t *testing.T) {
	// Pins the exact-match decision: ".gitignore" inside a longer string —
	// error wraps, log messages, tool descriptions — is prose, not a touch of
	// the file, and must never match.
	src := `package demo

import "fmt"

func describe() string {
	return "matches are filtered by .gitignore rules"
}

func cite() string {
	return "see https://git-scm.com/docs/gitignore"
}

func wrap(err error) error {
	return fmt.Errorf("read .gitignore: %w", err)
}
`
	fset, file := parseDelegationFixture(t, src)
	if sites := delegationSitesInFile(fset, "internal/demo", "prose.go", file, gitignoreDelegationRule(t)); len(sites) != 0 {
		t.Errorf("prose literals must not match, got %+v", sites)
	}
}
