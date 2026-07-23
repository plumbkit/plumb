package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// MoveSymbol relocates a whole top-level declaration from one file to another
// within the SAME directory/package. The move is atomic: the symbol's source is
// removed from source_uri and appended to destination_uri in a single
// all-or-nothing operation — if the destination write fails, the source is
// rolled back, so the declaration is never duplicated or lost.
//
// v1 is deliberately conservative: it does NOT rewrite references or imports, so
// any move that would change a symbol's package or import path (a different
// directory, or a different Go `package` clause) is REFUSED rather than applied
// half-correctly. See Description.
//
// Concurrency: Execute is safe for concurrent use; the apply path holds both
// files' per-path write locks across resolve, prepare, write, and bookkeeping.
type MoveSymbol struct {
	client   lsp.Client
	timeout  time.Duration
	topo     topologyStoreFn
	warmup   LSPWarmupFn  // may be nil; distinguishes a warming server from an unavailable one in the fallback banner
	ws       WorkspaceFn  // may be nil; anchors a workspace-relative uri to the pinned root
	cache    *cache.Cache // may be nil; evicted after a successful apply so the next query sees fresh symbols
	showDiff func() bool  // may be nil; resolves the show_write_diff toggle (defaults on)
	deps     WriteDeps
	hasDeps  bool
}

func NewMoveSymbol(client lsp.Client, timeout time.Duration) *MoveSymbol {
	return &MoveSymbol{client: client, timeout: timeout}
}

// WithCache wires the session symbol cache so a successful move evicts both
// files' entries (parity with edit_file/write_file). Nil-safe.
func (t *MoveSymbol) WithCache(c *cache.Cache) *MoveSymbol {
	t.cache = c
	return t
}

// WithTopologyFallback wires the topology index so the source symbol can be
// resolved from a fresh tree-sitter parse when the language server is
// unavailable. Nil-safe; returns the tool for chaining.
func (t *MoveSymbol) WithTopologyFallback(fn topologyStoreFn) *MoveSymbol {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the tree-sitter fallback banner says
// "still warming" instead of "LSP unavailable" while the server that owns the
// source file is completing its handshake. Nil-safe.
func (t *MoveSymbol) WithLSPWarmup(fn LSPWarmupFn) *MoveSymbol {
	t.warmup = fn
	return t
}

// WithWorkspace anchors a relative input uri to the pinned workspace. Nil-safe.
func (t *MoveSymbol) WithWorkspace(ws WorkspaceFn) *MoveSymbol {
	t.ws = ws
	return t
}

// WithShowWriteDiff wires the per-session show_write_diff resolver. Nil-safe.
func (t *MoveSymbol) WithShowWriteDiff(fn func() bool) *MoveSymbol {
	t.showDiff = fn
	return t
}

func (t *MoveSymbol) WithWriteDeps(deps WriteDeps) *MoveSymbol {
	t.deps = deps
	t.hasDeps = true
	if deps.Cache != nil {
		t.cache = deps.Cache
	}
	return t
}

func (*MoveSymbol) Name() string { return "move_symbol" }

func (*MoveSymbol) Description() string {
	return `Move a top-level declaration (function, method, type, const, or var) from one file to another within the SAME directory/package, atomically.

The symbol's full source — its declaration and, by default, its contiguous leading doc comment (include_doc_comment, default true) — is removed from source_uri and appended to destination_uri in a single all-or-nothing operation: if the destination write fails, the source is rolled back, so the declaration is never duplicated or lost. Locates the symbol via the LSP document-symbol tree, falling back to a fresh tree-sitter parse when the language server is cold or unavailable.

Scope (v1, conservative): source and destination must be in the SAME directory. plumb does NOT rewrite references or imports, so a move that would change a symbol's package or import path — a different directory, or (for Go) a different package clause — is REFUSED rather than applied half-correctly. Move within a package where references resolve unchanged; relocate across packages by hand.

destination_uri must already exist unless create_destination=true (a newly created Go file is seeded with the source file's package clause). Refuses when the symbol is not found, the name is ambiguous (disambiguate with a slash-separated name_path), the destination is missing without create_destination, or either path is outside the workspace. For Go, also refuses when source and destination carry different //go:build (or legacy +build) constraints, since moving a declaration between them would silently change what compiles per platform/tag.

Dry-run by default (dry_run=true): previews the unified diff of both files without writing. Set dry_run=false to apply.

Undo is per-file: reverting a move takes two undo_edit calls, one for source and one for destination, and the state between them is a transient duplicate of the moved declaration in both files.`
}

var moveSymbolSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "source_uri":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path of the file currently holding the symbol."},
    "name_path":{"type":"string","description":"Slash-separated symbol path within the source file (e.g. \"ClassName/methodName\", or just \"funcName\" for a top-level declaration)."},
    "destination_uri":{"type":"string","description":"Absolute path, file:// URI, or workspace-relative path of the file to move the declaration into. Must be in the SAME directory (package) as source_uri."},
    "include_doc_comment":{"type":"boolean","default":true,"description":"Move the symbol's contiguous leading doc comment along with it. Default true — a relocated declaration should keep its documentation."},
    "create_destination":{"type":"boolean","default":false,"description":"Create destination_uri if it does not exist. Default false (the destination must already exist). A newly created Go file is seeded with the source file's package clause."},
    "dry_run":{"type":"boolean","default":true,"description":"If true (default), preview the diff of both files only; do not write."},
    "dirty_ok":{"type":"boolean","default":false,"description":"Allow moving when either file has uncommitted changes. Default false — review/commit first, or pass true to proceed."}
  },
  "required":["source_uri","name_path","destination_uri"],
  "additionalProperties":false
}`)

func (*MoveSymbol) InputSchema() json.RawMessage { return moveSymbolSchema }

type moveSymbolArgs struct {
	SourceURI         string `json:"source_uri"`
	NamePath          string `json:"name_path"`
	DestinationURI    string `json:"destination_uri"`
	IncludeDocComment *bool  `json:"include_doc_comment,omitempty"`
	CreateDestination bool   `json:"create_destination,omitempty"`
	DryRun            *bool  `json:"dry_run,omitempty"`
	DirtyOK           bool   `json:"dirty_ok,omitempty"`
}

func parseMoveSymbolArgs(raw json.RawMessage) (moveSymbolArgs, error) {
	var a moveSymbolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("move_symbol: invalid arguments: %w", err)
	}
	if a.SourceURI == "" || a.NamePath == "" || a.DestinationURI == "" {
		return a, fmt.Errorf("move_symbol: source_uri, name_path, and destination_uri are required")
	}
	return a, nil
}

func (t *MoveSymbol) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseMoveSymbolArgs(raw)
	if err != nil {
		return "", err
	}
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()

	src := toFileURIAnchored(a.SourceURI, t.ws)
	dst := toFileURIAnchored(a.DestinationURI, t.ws)
	srcPath := paths.URIToPath(src)
	dstPath := paths.URIToPath(dst)

	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	includeDoc := true
	if a.IncludeDocComment != nil {
		includeDoc = *a.IncludeDocComment
	}
	return t.moveOrPreview(ctx, a, src, srcPath, dstPath, dryRun, includeDoc)
}

func (t *MoveSymbol) moveOrPreview(ctx context.Context, a moveSymbolArgs, src, srcPath, dstPath string, dryRun, includeDoc bool) (string, error) {
	deps := writeDepsPtr(t.hasDeps, &t.deps)
	if err := t.preflight(ctx, deps, srcPath, dstPath, dryRun, a.DirtyOK); err != nil {
		return "", err
	}
	if dryRun {
		plans, name, note, err := t.buildMovePlans(ctx, a, src, srcPath, dstPath, includeDoc)
		if err != nil {
			return "", err
		}
		return t.formatMove(plans, name, note, srcPath, dstPath, true, ""), nil
	}
	plans, name, note, baselines, err := t.applyMove(ctx, deps, a, src, srcPath, dstPath, includeDoc)
	if err != nil {
		return "", err
	}
	report := t.postWriteMove(ctx, deps, plans, baselines)
	return t.formatMove(plans, name, note, srcPath, dstPath, false, report), nil
}

// preflight gates a move before any resolve or write: the workspace boundary for
// BOTH files, then the structural refusals that need no I/O (same-file,
// cross-directory), and — in apply mode — the write-rate budget and the dirty
// guard. The cross-directory refusal is the honesty boundary: a move to another
// directory changes the symbol's package/import path, which v1 does not rewrite.
//
// Strict mode is deliberately NOT enforced (the rename_symbol precedent): the
// moved bytes are the symbol's OWN source relocated verbatim, not agent-authored
// content, so [edits] strict's "did this session read the file it authors into?"
// question does not apply. The dirty guard is what protects unreviewed work.
func (t *MoveSymbol) preflight(ctx context.Context, deps *WriteDeps, srcPath, dstPath string, dryRun, dirtyOK bool) error {
	if deps != nil {
		if err := deps.checkBoundary(srcPath); err != nil {
			return fmt.Errorf("move_symbol: %w", err)
		}
		if err := deps.checkBoundary(dstPath); err != nil {
			return fmt.Errorf("move_symbol: %w", err)
		}
	}
	if filepath.Clean(srcPath) == filepath.Clean(dstPath) {
		return fmt.Errorf("move_symbol: source_uri and destination_uri are the same file; use replace_symbol_body or insert_before/after_symbol to edit within one file")
	}
	if filepath.Dir(srcPath) != filepath.Dir(dstPath) {
		return fmt.Errorf("move_symbol: cross-directory move not supported in v1 (source dir %s, destination dir %s). Moving a declaration to a different directory changes its package/import path and would need reference rewriting across the workspace, which v1 does not do — move within the same directory/package, or relocate by hand and fix imports",
			filepath.Dir(srcPath), filepath.Dir(dstPath))
	}
	if dryRun {
		return nil
	}
	if deps != nil && deps.Limiter != nil && !deps.Limiter.Allow() {
		return rateLimitError("move_symbol", deps.Limiter)
	}
	if deps != nil && !dirtyOK {
		if dirtyBlocksWrite(ctx, *deps, srcPath) {
			return fmt.Errorf("move_symbol: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", srcPath)
		}
		if fileExists(dstPath) && dirtyBlocksWrite(ctx, *deps, dstPath) {
			return fmt.Errorf("move_symbol: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", dstPath)
		}
	}
	return nil
}

// applyMove holds both files' per-path locks across resolve → prepare → write →
// bookkeeping, so the resolved symbol range and the bytes it addresses cannot
// drift apart and the write/undo trackers record under the held lock. It returns
// the plans (for reporting), the moved symbol's name, any tree-sitter fallback
// banner, and the per-path pre-write diagnostics baselines.
func (t *MoveSymbol) applyMove(ctx context.Context, deps *WriteDeps, a moveSymbolArgs, src, srcPath, dstPath string, includeDoc bool) ([]movePlan, string, string, map[string]*diagBaseline, error) {
	unlocks := lockPaths([]string{srcPath, dstPath})
	defer unlockAll(unlocks)

	plans, name, note, err := t.buildMovePlans(ctx, a, src, srcPath, dstPath, includeDoc)
	if err != nil {
		return nil, "", "", nil, err
	}
	baselines := make(map[string]*diagBaseline, len(plans))
	if deps != nil {
		for _, p := range plans {
			baselines[p.path] = deps.capturePreWriteBaseline(protocol.FileURI(p.path))
		}
	}
	onApplied := func() {
		if deps == nil {
			return
		}
		for _, p := range plans {
			deps.recordWritten(p.path)
			deps.recordUndo(p.path, string(p.before), string(p.after), p.existedBefore, "move_symbol")
		}
	}
	if _, err := applyMovePlans(plans, onApplied); err != nil {
		return nil, "", "", nil, fmt.Errorf("move_symbol: applying move: %w", err)
	}
	return plans, name, note, baselines, nil
}

// postWriteMove runs the post-write pipeline for both modified files: the cheap
// non-blocking notify (LSP didChangeWatchedFiles, adapter hook, cache eviction,
// topology re-index) for each, then the differential diagnostics + quality
// report for each. Callers run it after applyMove has released the locks.
func (t *MoveSymbol) postWriteMove(ctx context.Context, deps *WriteDeps, plans []movePlan, baselines map[string]*diagBaseline) string {
	for _, p := range plans {
		uri := protocol.FileURI(p.path)
		if deps == nil {
			notifySymbolEditWritten(ctx, t.client, t.cache, p.path, uri)
			continue
		}
		semanticNotifyWritten(ctx, deps, t.client, t.cache, p.path, uri, "move_symbol")
	}
	if deps == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range plans {
		uri := protocol.FileURI(p.path)
		sb.WriteString(semanticPostWriteReport(ctx, deps, p.path, uri, string(p.before), string(p.after), baselines[p.path]))
	}
	return sb.String()
}

// buildMovePlans resolves the source symbol (LSP → tree-sitter fallback),
// extends the range to its doc comment when requested, and computes the two
// file plans: the source with the declaration removed, and the destination with
// it appended (creating the destination when allowed). It performs no writes,
// so it is safe to call for a dry-run preview and again under lock for apply.
func (t *MoveSymbol) buildMovePlans(ctx context.Context, a moveSymbolArgs, src, srcPath, dstPath string, includeDoc bool) ([]movePlan, string, string, error) {
	sym, viaFallback, err := t.resolveMoveTarget(ctx, src, a.NamePath)
	if err != nil {
		return nil, "", "", err
	}
	rng := sym.Range
	if includeDoc {
		rng.Start = docCommentStartPreferTopology(ctx, t.topo, src, a.NamePath, sym.Range.Start)
	}
	srcBefore, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("move_symbol: reading source: %w", err)
	}
	movedText, err := extractRange(srcBefore, rng)
	if err != nil {
		return nil, "", "", fmt.Errorf("move_symbol: %w", err)
	}
	srcAfter, err := applyTextEdits(srcBefore, []protocol.TextEdit{{Range: rng, NewText: ""}})
	if err != nil {
		return nil, "", "", fmt.Errorf("move_symbol: removing declaration from source: %w", err)
	}
	// extractRange above already validated rng.Start, so this offset lookup
	// cannot fail; it locates the removal seam for normalisation.
	if seam, ok := offsetForPosition(srcBefore, rng.Start); ok {
		srcAfter = normalizeRemovalSeam(srcAfter, seam)
	}
	srcPlan := movePlan{path: srcPath, before: srcBefore, after: srcAfter, mode: fileModeOr(srcPath, 0o644), existedBefore: true}

	destPlan, err := buildDestPlan(a, srcBefore, srcPath, dstPath, movedText)
	if err != nil {
		return nil, "", "", err
	}
	note := symbolEditFallbackNote(viaFallback, t.warmup, src)
	return []movePlan{srcPlan, destPlan}, sym.Name, note, nil
}

// resolveMoveTarget locates name_path in the source, refusing an ambiguous bare
// name (two top-level declarations share it — moving "the first" would be a
// silent guess). It then delegates to the shared LSP → tree-sitter resolver so
// the move works even when the language server is cold or cannot parse the file.
func (t *MoveSymbol) resolveMoveTarget(ctx context.Context, uri, namePath string) (*protocol.DocumentSymbol, bool, error) {
	if !strings.Contains(namePath, "/") && t.client != nil {
		if syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		}); err == nil {
			if m := resolveSymbolsByName(syms, namePath); len(m) > 1 {
				return nil, false, fmt.Errorf("move_symbol: %d symbols named %q in %s — ambiguous; v1 moves one declaration, disambiguate with a slash-separated name_path", len(m), namePath, paths.URIToPath(uri))
			}
		}
	}
	sym, viaFallback, err := resolveSymbolOrFallback(ctx, t.client, t.topo, uri, namePath)
	if err != nil {
		return nil, false, fmt.Errorf("move_symbol: %w", err)
	}
	return sym, viaFallback, nil
}

// buildDestPlan computes the destination file's after-content: the moved
// declaration appended, separated by a blank line. It enforces the same-package
// honesty boundary (a Go destination whose package clause differs is refused),
// and gates creation of a missing destination on create_destination.
func buildDestPlan(a moveSymbolArgs, srcBefore []byte, srcPath, dstPath, movedText string) (movePlan, error) {
	destBefore, existed, err := readIfExists(dstPath)
	if err != nil {
		return movePlan{}, fmt.Errorf("move_symbol: reading destination: %w", err)
	}
	if !existed && !a.CreateDestination {
		return movePlan{}, fmt.Errorf("move_symbol: destination %s does not exist; pass create_destination: true to create it", dstPath)
	}
	if existed {
		if err := checkSamePackage(srcPath, dstPath, srcBefore, destBefore); err != nil {
			return movePlan{}, err
		}
		if err := checkGoBuildTags(srcPath, dstPath, srcBefore, destBefore); err != nil {
			return movePlan{}, err
		}
	}
	seed := ""
	if !existed {
		seed = destSeed(srcPath, srcBefore)
	}
	mode := os.FileMode(0o644)
	if existed {
		mode = fileModeOr(dstPath, 0o644)
	}
	return movePlan{
		path:          dstPath,
		before:        destBefore,
		after:         appendDeclaration(destBefore, movedText, seed, existed),
		mode:          mode,
		existedBefore: existed,
	}, nil
}

// destSeed returns the preamble a newly created destination needs so it is
// valid on its own — for Go, the source file's package clause plus a blank line.
// Empty for other languages (v1 appends the bare declaration).
func destSeed(srcPath string, srcBefore []byte) string {
	if !isGoFile(srcPath) {
		return ""
	}
	if pkg := goPackageClause(srcBefore); pkg != "" {
		return pkg + "\n\n"
	}
	return ""
}

// appendDeclaration returns dest with decl appended, normalised to a single
// trailing newline on the declaration and separated from any prior content by a
// blank line. seed prefixes a newly created (or empty) destination.
func appendDeclaration(dest []byte, decl, seed string, existed bool) []byte {
	body := strings.TrimRight(decl, "\n") + "\n"
	if !existed || len(dest) == 0 {
		return []byte(seed + body)
	}
	s := string(dest)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if !strings.HasSuffix(s, "\n\n") {
		s += "\n"
	}
	return []byte(s + body)
}

// extractRange returns the verbatim source bytes covered by rng, using the same
// byte-offset resolution as the edit applier (so an LSP or tree-sitter range
// resolves identically to the deletion that removes it).
func extractRange(data []byte, rng protocol.Range) (string, error) {
	startOff, ok := offsetForPosition(data, rng.Start)
	if !ok {
		return "", fmt.Errorf("symbol start position out of range: line %d char %d", rng.Start.Line, rng.Start.Character)
	}
	endOff, ok := endOffsetForPosition(data, rng.End)
	if !ok {
		return "", fmt.Errorf("symbol end position out of range: line %d char %d", rng.End.Line, rng.End.Character)
	}
	if startOff > endOff {
		return "", fmt.Errorf("symbol start after end")
	}
	return string(data[startOff:endOff]), nil
}

func readIfExists(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileModeOr(path string, def os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		if m := info.Mode().Perm(); m != 0 {
			return m
		}
	}
	return def
}

func (t *MoveSymbol) formatMove(plans []movePlan, name, note, srcPath, dstPath string, dryRun bool, report string) string {
	var sb strings.Builder
	sb.WriteString(note)
	if dryRun {
		sb.WriteString("DRY RUN — no files modified.\n\n")
		fmt.Fprintf(&sb, "Would move %q from %s to %s\n", name, srcPath, dstPath)
	} else {
		fmt.Fprintf(&sb, "Moved %q from %s to %s\n", name, srcPath, dstPath)
	}
	if resolveShowDiff(t.showDiff) {
		for _, p := range plans {
			d := unifiedDiff(p.path, string(p.before), string(p.after))
			if d == "" {
				continue
			}
			sb.WriteString("\n")
			sb.WriteString(d)
			sb.WriteString("\n")
		}
	}
	if report != "" {
		sb.WriteString(report)
	}
	if dryRun {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}
	return sb.String()
}

// movePlan is one file's share of a move: its pre- and post-move bytes, its
// mode, and whether it existed before (a created destination is removed on
// rollback rather than restored).
type movePlan struct {
	path          string
	before        []byte
	after         []byte
	mode          os.FileMode
	existedBefore bool
}

// applyMovePlans writes each plan in order and rolls every prior write back on a
// mid-sequence failure — a created file is removed, an existing file restored to
// its pre-move bytes — keeping a two-file move all-or-nothing at the filesystem
// level. onApplied runs after all writes succeed. The caller holds the per-path
// locks for every plan (see applyMove); this helper performs no locking so it is
// also directly unit-testable.
func applyMovePlans(plans []movePlan, onApplied func()) ([]string, error) {
	var written []movePlan
	for _, p := range plans {
		if _, err := safeWrite(p.path, p.after, p.mode); err != nil {
			if rbErr := rollbackMove(written); rbErr != nil {
				return nil, fmt.Errorf("writing %s: %w; rollback failed: %v", p.path, err, rbErr)
			}
			return nil, fmt.Errorf("writing %s: %w", p.path, err)
		}
		written = append(written, p)
	}
	if onApplied != nil {
		onApplied()
	}
	out := make([]string, len(plans))
	for i, p := range plans {
		out[i] = p.path
	}
	return out, nil
}

func rollbackMove(written []movePlan) error {
	var errs []string
	for i := len(written) - 1; i >= 0; i-- {
		p := written[i]
		if !p.existedBefore {
			if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Sprintf("%s: %v", p.path, err))
			}
			continue
		}
		if _, err := safeWrite(p.path, p.before, p.mode); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.path, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
