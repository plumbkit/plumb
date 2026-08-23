package topology

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// This file is the admission gate for function-level, cross-file call answers,
// and the wording plumb uses when it will not give one.
//
// The rule is deliberately two POSITIVE conditions and nothing else. An earlier
// Go-only feature spent three review rounds oscillating between a guard that was
// too eager (an edge-count term told a small but genuinely Go workspace it was
// not Go) and one that was too lax (any signal derivable from a user's repo can
// be produced by one stray file of a language plumb cannot serve). Neither
// failure is reachable from "is the language in a compile-time set, and does the
// index hold a package node for it".

// supportedCallGraphLanguages is the compile-time set of languages whose call
// sites this resolver can follow across a file boundary. It is a constant, NOT
// something derived from the index: that is what makes it unspoofable — no
// property of a user's repository can add a language to it.
//
// Go is alone here because Go is the only language whose extractor emits all
// three ingredients a package-qualified resolver needs: a package node, an
// import node carrying the full import path, and a call whose qualifier is
// separable from its callee.
var supportedCallGraphLanguages = map[string]struct{}{
	"go": {},
}

// callGraphSupportableWithWork are the languages whose call sites already carry
// a separable qualifier and which therefore could be served later. Saying so is
// only honest for these: for swift, c, cpp, objc, ruby, dart and bash the design
// does not fit the language at all, and promising them a queue would be a lie
// with a long half-life.
var callGraphSupportableWithWork = map[string]struct{}{
	"php": {}, "elixir": {}, "scala": {}, "csharp": {}, "zig": {}, "lua": {},
	"typescript": {}, "tsx": {}, "javascript": {}, "python": {}, "rust": {},
}

// CallGraphSubject is what a caller asked about. The SUBJECT's language decides
// which admission is consulted — never the workspace's "primary" language, which
// is a threshold in disguise and is exactly how a repo that is 90% TypeScript
// with one tools/gen.go ends up being answered as a Go project.
type CallGraphSubject struct {
	// Language is the subject node's (or file's) language.
	Language string
	// Path is the subject's workspace-relative file, when there is one. It is
	// used only to count the intra-file call edges the refusal offers instead.
	Path string
}

// CallGraphAdmission is the verdict for one subject.
//
// Exactly one of Refusal and ScopeNote is the thing to show: a refused subject
// gets Refusal, an admitted one gets ScopeNote (which may be empty when there is
// nothing to scope). Admitted is the field to branch on — never the emptiness of
// a string.
type CallGraphAdmission struct {
	Language string
	Admitted bool
	// Refusal is the message for a subject in a language that cannot be served.
	// Empty when Admitted.
	Refusal string
	// ScopeNote labels an admitted answer: which languages were left out of it,
	// and that they are out of scope rather than caller-free. Empty when the
	// workspace holds only the admitted language.
	ScopeNote string
	// EmptyResultNote is set on an ADMITTED language whose subgraph yielded no
	// cross-file call edge. It is a label on a correct answer, not a refusal:
	// telling a small but genuinely Go workspace that it is not a Go workspace is
	// the concrete regression this field exists to make impossible.
	EmptyResultNote string
	// OutOfScope counts indexed files per language other than the admitted one.
	OutOfScope map[string]int
	// IntraFileCalls is the number of intra-file call edges available for the
	// subject — the coarser answer the refusal points at.
	IntraFileCalls int
	// HasPackageSignal records the second, positive half of the gate.
	HasPackageSignal bool
}

// AdmitCallGraph applies the admission rule to one subject.
//
// Language L is admitted iff L is in supportedCallGraphLanguages AND the index
// holds at least one package node with language = L. Both terms positive: no
// edge count, no ratio, no "primary language", no threshold. A supported
// language with no package node in the index is not admitted, because the thing
// that would be traversed is not there — but that is a fact about the index, not
// a claim that the workspace has no callers.
func AdmitCallGraph(ctx context.Context, db *sql.DB, subject CallGraphSubject) (CallGraphAdmission, error) {
	lang := strings.ToLower(strings.TrimSpace(subject.Language))
	a := CallGraphAdmission{Language: lang}
	_, supported := supportedCallGraphLanguages[lang]

	hasPkg, err := hasPackageNode(ctx, db, lang)
	if err != nil {
		return a, err
	}
	a.HasPackageSignal = hasPkg
	a.IntraFileCalls, err = intraFileCallEdges(ctx, db, lang, subject.Path)
	if err != nil {
		return a, err
	}
	a.Admitted = supported && hasPkg
	if !a.Admitted {
		a.Refusal = callGraphRefusal(lang, a.IntraFileCalls, subject.Path != "")
		return a, nil
	}
	a.OutOfScope, err = otherLanguageFiles(ctx, db, lang)
	if err != nil {
		return a, err
	}
	a.ScopeNote, err = callGraphScopeNote(ctx, db, lang, a.OutOfScope)
	if err != nil {
		return a, err
	}
	resolved, err := resolvedEdgeCount(ctx, db, lang)
	if err != nil {
		return a, err
	}
	if resolved == 0 {
		a.EmptyResultNote = CallGraphEmptyResultNote(lang)
	}
	return a, nil
}

// resolvedEdgeCount counts the cross-file call edges the resolver produced for
// lang. It is read ONLY to choose the empty-result label — never as an admission
// term. An edge count in the gate is what made a real Go workspace refusable.
func resolvedEdgeCount(ctx context.Context, db *sql.DB, lang string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM topology_edges e
           JOIN topology_nodes a ON a.id = e.from_id
          WHERE e.source = ? AND a.language = ?`, callResolverSource, lang).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("topology: call-graph resolved count: %w", err)
	}
	return n, nil
}

func hasPackageNode(ctx context.Context, db *sql.DB, lang string) (bool, error) {
	if lang == "" {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM topology_nodes WHERE kind = ? AND language = ? LIMIT 1`,
		string(KindPackage), lang).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("topology: call-graph admission: %w", err)
	}
	return true, nil
}

// intraFileCallEdges counts the extractor-emitted call edges the refusal offers
// as the coarser answer: for the subject's own file when there is one, and for
// the language's whole indexed set otherwise.
//
// Resolver edges are excluded by source rather than left out by circumstance.
// They cannot exist for a refused language today — the resolver runs only for an
// admitted one — but the string this number goes into says "intra-file call edges
// only", and a count that is right only while a separate invariant holds is a
// claim waiting to become false.
func intraFileCallEdges(ctx context.Context, db *sql.DB, lang, path string) (int, error) {
	var n int
	var err error
	if path != "" {
		err = db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM topology_edges e
               JOIN topology_nodes fn ON fn.id = e.from_id
               JOIN topology_files f  ON f.id = fn.file_id
              WHERE e.kind = ? AND e.source <> ? AND f.path = ?`,
			string(EdgeCalls), callResolverSource, path).Scan(&n)
	} else {
		err = db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM topology_edges e
               JOIN topology_nodes fn ON fn.id = e.from_id
              WHERE e.kind = ? AND e.source <> ? AND fn.language = ?`,
			string(EdgeCalls), callResolverSource, lang).Scan(&n)
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("topology: call-graph intra-file count: %w", err)
	}
	return n, nil
}

// otherLanguageFiles counts indexed files per language other than admitted.
// Files with no language (unrecognised, uncovered) are not counted: naming them
// as an out-of-scope language would invent one.
func otherLanguageFiles(ctx context.Context, db *sql.DB, admitted string) (map[string]int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT language, COUNT(*) FROM topology_files
          WHERE language <> '' AND language <> ? AND content_hash <> ''
          GROUP BY language`, admitted)
	if err != nil {
		return nil, fmt.Errorf("topology: call-graph scope: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var l string
		var n int
		if err := rows.Scan(&l, &n); err != nil {
			return nil, fmt.Errorf("topology: call-graph scope scan: %w", err)
		}
		out[l] = n
	}
	return out, rows.Err()
}

// callGraphScopeNote is the label on an ADMITTED answer in a polyglot workspace.
// It is not a refusal: it states that the other languages' files were left out
// of this analysis, which is a different fact from their having no callers.
func callGraphScopeNote(ctx context.Context, db *sql.DB, lang string, others map[string]int) (string, error) {
	if len(others) == 0 {
		return "", nil
	}
	var pkgs, files int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM topology_nodes WHERE kind = ? AND language = ?`,
		string(KindPackage), lang).Scan(&pkgs); err != nil {
		return "", fmt.Errorf("topology: call-graph scope packages: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM topology_files WHERE language = ? AND content_hash <> ''`,
		lang).Scan(&files); err != nil {
		return "", fmt.Errorf("topology: call-graph scope files: %w", err)
	}
	names := make([]string, 0, len(others))
	total := 0
	for l, n := range others {
		names = append(names, l)
		total += n
	}
	sort.Strings(names)
	return fmt.Sprintf(
		"Scope: function-level call edges cover the %s subgraph only — %d %s, %d %s. "+
			"%d %s in %s are out of scope for this analysis, not unreachable and not caller-free; "+
			"ask about them with `find_references` (or see the per-language note above).",
		lang,
		pkgs, textfmt.Plural(pkgs, "package", "packages"),
		files, textfmt.Plural(files, "file", "files"),
		total, textfmt.Plural(total, "file", "files"),
		strings.Join(names, ", ")), nil
}

// CallGraphEmptyResultNote is the label for an ADMITTED language whose subgraph
// yielded no cross-file call edge at all. It is deliberately not a refusal: a
// small, genuinely Go workspace whose every qualified call goes to the standard
// library has a correct and complete answer, and telling it that it is not a Go
// workspace was the concrete regression this wording replaces.
func CallGraphEmptyResultNote(lang string) string {
	return fmt.Sprintf(
		"Note: this workspace's %s subgraph produced no cross-file call edges — every qualified "+
			"call in it resolves outside the module (standard library or third-party), which a "+
			"package-qualified resolver deliberately does not follow. The intra-file edges below "+
			"are complete and correct.", lang)
}

// callGraphRefusal builds the message for a subject plumb will not answer.
// It splits on whether the language has an LSP adapter, because the honest
// alternative differs: with a server, find_references really does answer the
// cross-file question; without one, there is no cross-file caller answer to give
// and the string must not imply otherwise. Package-level reachability is NOT
// offered as the coarser answer — it is itself gated to the same language set,
// so a refused subject is refused there too.
func callGraphRefusal(lang string, intraFileCalls int, haveFile bool) string {
	name := lang
	if name == "" {
		name = "this language"
	}
	where := "in the indexed " + name + " files"
	if haveFile {
		where = "in this file"
	}
	count := fmt.Sprintf("%d %s", intraFileCalls, where)

	var b strings.Builder
	if callGraphHasLSPAdapter(lang) {
		fmt.Fprintf(&b,
			"`call_hierarchy`/`topology_impact`: cross-file call edges are not available for %s. "+
				"plumb's %s index carries intra-file call edges only (%s), because resolving a call "+
				"across files needs a module identity and a per-file import path that the %s extractor "+
				"does not record. What does work today: `find_references` and `call_hierarchy` answer "+
				"this question for %s through the language server, which is cross-file and type-aware — "+
				"use those. Also available without a server: `file_outline`, `topology_explore` "+
				"(containment and import neighbourhood), and `search_in_files` for an exact-text sweep. "+
				"Function-level call answers from the topology index are Go-only today.",
			name, name, count, name, name)
	} else {
		fmt.Fprintf(&b,
			"`call_hierarchy`/`topology_impact`: cross-file call edges are not available for %s, and "+
				"plumb has no %s language server adapter, so there is no cross-file caller answer to "+
				"give you. What the index does hold for %s: intra-file call edges (%s), containment, "+
				"and import nodes — reachable through `file_outline` and `topology_explore`. For a "+
				"cross-file sweep, use `search_in_files` on the symbol name and read the hits; it is "+
				"exact over current file contents, and it is the honest tool for this question here. "+
				"Function-level call answers from the topology index are Go-only today.",
			name, name, name, count)
	}
	if _, ok := callGraphSupportableWithWork[lang]; ok {
		fmt.Fprintf(&b,
			" %s can support this: its call sites already carry a separable qualifier, and what is "+
				"missing is module identity in the index. It is tracked and not scheduled.", name)
	}
	return b.String()
}

// callGraphHasLSPAdapter reports whether plumb ships a language server adapter
// for lang, which is what decides whether the refusal may point at
// find_references as a real cross-file alternative.
func callGraphHasLSPAdapter(lang string) bool {
	l, ok := langsupport.ByName(lang)
	return ok && l.LSPAdapter != ""
}
