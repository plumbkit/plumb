package treesitter

import (
	"sync"

	"github.com/odvcencio/gotreesitter/grammars"
)

// gotreesitter v0.19.1 ships the TypeScript/TSX external *scanners* but not the
// external *lex-states* tables (`ts_external_scanner_states`), so the GLR parser
// cannot resolve TypeScript's arrow-vs-paren ambiguity and typed arrow params
// cascade ERROR nodes (see docs/internal/treesitter-plan.md, "Phase 1 fidelity
// finding"). The fix is to supply the missing tables via the grammars package's
// exported RegisterExternalLexStates — no fork required.
//
// The two tables below were extracted from the upstream tree-sitter-typescript
// `parser.c` at the EXACT commit gotreesitter v0.19.1 embeds
// (75b3874edb2dc714fb1fd77a32013d0f8699989f, recorded in
// grammars/grammar_updates.json) using gotreesitter's bundled `ts2go` tool, then
// read back out of the generated grammar blob with `gotreesitter.LoadLanguage`.
// They are therefore column-aligned with the embedded parse tables.
// EXTERNAL_TOKEN_COUNT is 10 for both grammars.
//
// IMPORTANT: these tables are valid only for the embedded grammar version. If a
// future gotreesitter bump changes the TypeScript/TSX `parser.c`, the external
// lex-state IDs may desync — TestTypeScript_TypedArrowNoCascade is the guard: it
// re-parses a typed-arrow utility module and fails if the cascade returns,
// signalling the tables must be regenerated. To regenerate, run
//
//	go run github.com/odvcencio/gotreesitter/cmd/ts2go -input <typescript/src/parser.c> \
//	    -output /tmp/ts.go -name typescript -compact=false
//	# then LoadLanguage("/tmp/grammar_blobs/typescript.bin").ExternalLexStates
//
// against the parser.c at the new pinned commit (and likewise for tsx).

// typescriptExternalLexStates mirrors tree-sitter-typescript's
// ts_external_scanner_states (11 rows x 10 external-token columns).
var typescriptExternalLexStates = [][]bool{
	{false, false, false, false, false, false, false, false, false, false},
	{true, true, true, true, true, true, false, true, true, true},
	{false, false, false, true, false, false, false, false, false, false},
	{false, false, true, true, true, false, false, false, false, false},
	{true, false, true, true, true, false, false, false, false, false},
	{true, false, false, true, false, false, false, false, false, false},
	{true, false, false, true, false, false, false, false, true, false},
	{false, true, false, true, false, true, false, false, false, false},
	{false, true, false, true, false, false, false, false, false, false},
	{false, false, false, true, false, true, false, false, false, false},
	{false, false, false, true, false, false, true, false, false, false},
}

// tsxExternalLexStates mirrors tree-sitter-typescript's TSX
// ts_external_scanner_states (12 rows x 10 external-token columns).
var tsxExternalLexStates = [][]bool{
	{false, false, false, false, false, false, false, false, false, false},
	{true, true, true, true, true, true, false, true, true, true},
	{false, false, false, true, false, false, false, false, false, false},
	{false, false, true, true, true, false, false, false, false, false},
	{true, false, true, true, true, false, false, false, false, false},
	{true, false, false, true, false, false, false, false, false, false},
	{true, false, false, true, false, false, false, false, true, false},
	{false, false, false, true, false, false, false, true, false, false},
	{false, true, false, true, false, true, false, false, false, false},
	{false, true, false, true, false, false, false, false, false, false},
	{false, false, false, true, false, true, false, false, false, false},
	{false, false, false, true, false, false, true, false, false, false},
}

var tsLexStatesOnce sync.Once

// registerTSLexStates supplies the missing TypeScript/TSX external lex-states to
// the grammars registry. It must run before the first TypescriptLanguage() /
// TsxLanguage() load so the loader attaches the tables to the decoded grammar
// (grammars/embedded_loader.go). The TypeScript extractor's constructor is the
// only caller that loads those grammars, so registering there (before the load)
// is sufficient and avoids package-init side effects.
func registerTSLexStates() {
	tsLexStatesOnce.Do(func() {
		grammars.RegisterExternalLexStates("typescript", typescriptExternalLexStates)
		grammars.RegisterExternalLexStates("tsx", tsxExternalLexStates)
	})
}
