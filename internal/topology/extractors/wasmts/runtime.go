package wasmts

import (
	"context"
	_ "embed"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ts.wasm bundles the canonical tree-sitter runtime + tree-sitter-typescript
// (typescript + tsx) grammars, compiled to wasm32-wasi by csrc/build.sh. It is
// committed so building plumb needs only Go + wazero (no C toolchain). See
// csrc/NOTICE.md for provenance and regeneration.
//
//go:embed ts.wasm
var tsWasm []byte

// runtime wraps one wazero instance of the bundled tree-sitter wasm.
//
// Concurrency: NOT safe for concurrent use. tree-sitter nodes are pointers into
// the module's shared linear memory, so a parse and the tree walk that follows
// it must run without interleaving. parse() holds mu for that whole duration.
// One runtime per process is the intended usage; the extractors share one and
// serialise their Extract calls through it. Parsing TS/TSX is sub-millisecond,
// so the serialisation is immaterial at index time.
type runtime struct {
	mu  sync.Mutex
	wzr wazero.Runtime
	mod api.Module
	ctx context.Context //nolint:containedctx // active only during a locked parse

	malloc, free, strlen api.Function

	parserNew, parserParse, parserSetLang, parserDelete api.Function
	treeRoot, treeDelete                                api.Function

	nChildCount, nChild, nNamedChild, nChildByField api.Function
	nType, nStartByte, nEndByte                     api.Function
	nIsNull                                         api.Function

	tsLang, tsxLang uint64

	arena []uint64 // node/string ptrs allocated during the current parse
	err   error    // sticky wazero error for the current parse
}

// newRuntime compiles and instantiates the embedded tree-sitter wasm once.
func newRuntime(ctx context.Context) (*runtime, error) {
	wzr := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, wzr)

	compiled, err := wzr.CompileModule(ctx, tsWasm)
	if err != nil {
		return nil, fmt.Errorf("compiling ts.wasm: %w", err)
	}
	mod, err := wzr.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiating ts.wasm: %w", err)
	}

	r := &runtime{
		wzr:           wzr,
		mod:           mod,
		malloc:        mod.ExportedFunction("malloc"),
		free:          mod.ExportedFunction("free"),
		strlen:        mod.ExportedFunction("strlen"),
		parserNew:     mod.ExportedFunction("ts_parser_new"),
		parserParse:   mod.ExportedFunction("ts_parser_parse_string"),
		parserSetLang: mod.ExportedFunction("ts_parser_set_language"),
		parserDelete:  mod.ExportedFunction("ts_parser_delete"),
		treeRoot:      mod.ExportedFunction("ts_tree_root_node"),
		treeDelete:    mod.ExportedFunction("ts_tree_delete"),
		nChildCount:   mod.ExportedFunction("ts_node_child_count"),
		nChild:        mod.ExportedFunction("ts_node_child"),
		nNamedChild:   mod.ExportedFunction("ts_node_named_child"),
		nChildByField: mod.ExportedFunction("ts_node_child_by_field_name"),
		nType:         mod.ExportedFunction("ts_node_type"),
		nStartByte:    mod.ExportedFunction("ts_node_start_byte"),
		nEndByte:      mod.ExportedFunction("ts_node_end_byte"),
		nIsNull:       mod.ExportedFunction("ts_node_is_null"),
	}

	lang, err := r.loadLang(ctx, "tree_sitter_typescript")
	if err != nil {
		return nil, err
	}
	r.tsLang = lang
	if lang, err = r.loadLang(ctx, "tree_sitter_tsx"); err != nil {
		return nil, err
	}
	r.tsxLang = lang
	return r, nil
}

func (r *runtime) loadLang(ctx context.Context, export string) (uint64, error) {
	fn := r.mod.ExportedFunction(export)
	if fn == nil {
		return 0, fmt.Errorf("wasm export %q missing", export)
	}
	res, err := fn.Call(ctx)
	if err != nil {
		return 0, fmt.Errorf("calling %s: %w", export, err)
	}
	return res[0], nil
}

// parse parses src with the given language pointer and invokes visit with the
// root node, all under the runtime lock. Every allocation made during the parse
// (the tree, the source buffer, and node result structs) is freed before parse
// returns, so memory does not grow across calls. visit must not retain nodes.
func (r *runtime) parse(ctx context.Context, lang uint64, src []byte, visit func(node)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ctx = ctx
	r.err = nil
	r.arena = r.arena[:0]
	defer r.freeArena()

	parser, err := r.parserNew.Call(ctx)
	if err != nil {
		return fmt.Errorf("ts_parser_new: %w", err)
	}
	defer r.parserDelete.Call(ctx, parser[0]) //nolint:errcheck // cleanup

	if _, err = r.parserSetLang.Call(ctx, parser[0], lang); err != nil {
		return fmt.Errorf("ts_parser_set_language: %w", err)
	}

	srcPtr, err := r.writeBytes(src)
	if err != nil {
		return err
	}
	tree, err := r.parserParse.Call(ctx, parser[0], 0, srcPtr, uint64(len(src)))
	if err != nil {
		return fmt.Errorf("ts_parser_parse_string: %w", err)
	}
	if tree[0] == 0 {
		return nil
	}
	defer r.treeDelete.Call(ctx, tree[0]) //nolint:errcheck // cleanup

	rootPtr := r.allocNode()
	if _, err = r.treeRoot.Call(ctx, rootPtr, tree[0]); err != nil {
		return fmt.Errorf("ts_tree_root_node: %w", err)
	}
	visit(node{r: r, ptr: rootPtr})
	return r.err
}

// allocNode reserves 24 bytes (sizeof TSNode) and records the pointer for
// bulk-free at parse end.
func (r *runtime) allocNode() uint64 {
	res, err := r.malloc.Call(r.ctx, 24)
	if err != nil {
		r.err = err
		return 0
	}
	r.arena = append(r.arena, res[0])
	return res[0]
}

// writeBytes copies b into wasm memory and records the pointer for bulk-free.
func (r *runtime) writeBytes(b []byte) (uint64, error) {
	res, err := r.malloc.Call(r.ctx, uint64(len(b)))
	if err != nil {
		return 0, fmt.Errorf("malloc: %w", err)
	}
	ptr := res[0]
	r.arena = append(r.arena, ptr)
	if !r.mod.Memory().Write(abiU32(ptr), b) {
		return 0, fmt.Errorf("writing %d bytes to wasm memory at %d", len(b), ptr)
	}
	return ptr, nil
}

func (r *runtime) freeArena() {
	for _, p := range r.arena {
		r.free.Call(r.ctx, p) //nolint:errcheck // best-effort cleanup
	}
	r.arena = r.arena[:0]
}

// call invokes a wasm function, recording any error on r.err and returning 0 so
// the walk can continue harmlessly. Exported tree-sitter accessors do not fail
// for valid nodes, so a non-nil r.err signals a genuine wasm-level fault.
func (r *runtime) call(fn api.Function, args ...uint64) uint64 {
	if r.err != nil {
		return 0
	}
	res, err := fn.Call(r.ctx, args...)
	if err != nil {
		r.err = err
		return 0
	}
	if len(res) == 0 {
		return 0
	}
	return res[0]
}

// abiInt narrows a wazero call result to int. tree-sitter byte offsets, child
// counts and node indices are all uint32 in the C ABI; wazero widens every
// result to uint64, so the value is known to fit.
func abiInt(v uint64) int { return int(uint32(v)) } //nolint:gosec // G115: tree-sitter ABI value fits in uint32

// abiU32 narrows a wasm pointer/length to uint32 (the wasm32 address space is
// 32-bit, so malloc results and string lengths always fit).
func abiU32(v uint64) uint32 { return uint32(v) } //nolint:gosec // G115: wasm32 pointer/length fits in uint32

func (r *runtime) readCString(ptr uint64) string {
	if ptr == 0 || r.err != nil {
		return ""
	}
	n := r.call(r.strlen, ptr)
	if n == 0 {
		return ""
	}
	b, ok := r.mod.Memory().Read(abiU32(ptr), abiU32(n))
	if !ok {
		return ""
	}
	return string(b)
}
