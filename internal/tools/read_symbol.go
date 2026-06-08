package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var readSymbolSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file containing the symbol"
    },
    "uri": {
      "type": "string",
      "description": "Alias for path (file:// URI or absolute path). Used only when path is omitted."
    },
    "name": {
      "type": "string",
      "description": "Exact symbol name. Accepts plain name (e.g. \"handleConn\") or dotted ReceiverType.MethodName form (e.g. \"Model.renderDashboard\")."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`)

// ReadSymbol returns the source body of a named symbol in one call,
// collapsing the list_symbols + read_file two-round-trip pattern.
// When multiple symbols share the name, all matches are returned.
// The mtime header matches read_file so callers can pass it as expected_mtime.
//
// Concurrency: Execute is safe for concurrent use.
type ReadSymbol struct {
	client       lsp.Client
	cache        *cache.Cache
	ttl          time.Duration
	timeout      time.Duration
	tracker      *ReadTracker
	topo         topologyStoreFn
	guard        BoundaryGuard
	clientNameFn func() string       // may be nil; gates the edit-lane hint to conflict-prone clients
	outsideFn    func(string) string // may be nil; returns a root label when the path is outside the workspace
}

func NewReadSymbol(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration, tracker *ReadTracker) *ReadSymbol {
	return &ReadSymbol{client: client, cache: c, ttl: ttl, timeout: timeout, tracker: tracker}
}

// WithTopologyFallback wires the topology index so read_symbol can locate the
// symbol from a fresh tree-sitter parse when the language server is
// unavailable. Nil-safe; returns the tool for chaining.
func (t *ReadSymbol) WithTopologyFallback(fn topologyStoreFn) *ReadSymbol {
	t.topo = fn
	return t
}

func (t *ReadSymbol) WithBoundary(guard BoundaryGuard) *ReadSymbol {
	t.guard = guard
	return t
}

// WithClient wires the MCP client-name accessor so read_symbol can append the
// edit-lane hint only for clients whose native Edit tool conflicts with plumb's
// read-state (see edit_lane.go). read_symbol returns a symbol body the agent is
// about to edit, so it is as much an edit precursor as read_file. Nil-safe;
// without it no hint is emitted.
func (t *ReadSymbol) WithClient(fn func() string) *ReadSymbol {
	t.clientNameFn = fn
	return t
}

// WithOutsideLabel wires an accessor returning a root label when a path lies
// outside the workspace (read-only dependency or configured read root), so
// read_symbol can annotate out-of-workspace reads as not editable. Nil-safe.
func (t *ReadSymbol) WithOutsideLabel(fn func(string) string) *ReadSymbol {
	t.outsideFn = fn
	return t
}

func (t *ReadSymbol) outsideLabel(path string) string {
	if t.outsideFn == nil {
		return ""
	}
	return t.outsideFn(path)
}

func (t *ReadSymbol) Name() string                 { return "read_symbol" }
func (t *ReadSymbol) InputSchema() json.RawMessage { return readSymbolSchema }
func (t *ReadSymbol) Description() string {
	return "Read the source body of a named symbol (function, method, type) in one call — " +
		"no native Claude Code equivalent for this LSP-backed lookup. " +
		"Accepts plain name or dotted ReceiverType.MethodName form. " +
		"Returns all matches when the name is ambiguous. " +
		"Prefer this over a list_symbols + read_file pair for targeted symbol reads. " +
		"Falls back to a tree-sitter parse when the language server is cold or absent."
}

type readSymbolArgs struct {
	Path string `json:"path"`
	URI  string `json:"uri"`
	Name string `json:"name"`
}

func (t *ReadSymbol) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseReadSymbolArgs(raw)
	if err != nil {
		return "", err
	}
	fpath, uri := resolveReadSymbolPaths(a.Path)
	if err := t.guard.check(fpath); err != nil {
		return "", fmt.Errorf("read_symbol: %w", err)
	}
	ctx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	syms, err := t.fetchReadSymbolSymbols(ctx, uri)
	if err != nil {
		if fb, ok := t.topologyReadFallback(ctx, fpath, uri, a.Name); ok {
			return fb, nil
		}
		return "", err
	}
	matches := resolveSymbolsByName(syms, a.Name)
	if len(matches) == 0 {
		// The LSP answered but did not resolve the name (commonly a cold server,
		// or a bare method name it indexes only as a qualified symbol). Try the
		// structural Map before giving up — the Go extractor names methods by their
		// bare name, so it resolves what the LSP missed.
		if fb, ok := t.topologyReadFallback(ctx, fpath, uri, a.Name); ok {
			return fb, nil
		}
		return t.noSymbolMessage(a.Name, fpath), nil
	}
	return t.formatReadSymbolResult(fpath, a.Name, matches)
}

// topologyReadFallback locates the named symbol from a fresh tree-sitter parse
// when the language server cannot answer, and reads its source the same way the
// LSP path does. ok is false when topology is unavailable or has no match, so
// the caller surfaces the original LSP error.
func (t *ReadSymbol) topologyReadFallback(ctx context.Context, fpath, uri, name string) (string, bool) {
	nodes, ok := freshTopologyNodes(ctx, t.topo, uri)
	if !ok {
		return "", false
	}
	matchNodes := topologyNodesByName(nodes, name)
	if len(matchNodes) == 0 {
		return "", false
	}
	lines := fileLines(fpath)
	matches := make([]protocol.DocumentSymbol, 0, len(matchNodes))
	for _, n := range matchNodes {
		matches = append(matches, nodeToDocSymbol(n, lines))
	}
	out, err := t.formatReadSymbolResult(fpath, name, matches)
	if err != nil {
		return "", false
	}
	return topologyFallbackNote + "\n" + out, true
}

func parseReadSymbolArgs(raw json.RawMessage) (readSymbolArgs, error) {
	var a readSymbolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("read_symbol: invalid arguments: %w", err)
	}
	if a.Path == "" {
		a.Path = a.URI // uri is an alias for path
	}
	if a.Path == "" {
		return a, fmt.Errorf("read_symbol: path (or uri) is required")
	}
	if a.Name == "" {
		return a, fmt.Errorf("read_symbol: name is required")
	}
	return a, nil
}

// noSymbolMessage renders the not-found message, adding a hint when the file is
// outside the workspace — neither the LSP nor the topology index covers those,
// so read_file is the right tool.
func (t *ReadSymbol) noSymbolMessage(name, fpath string) string {
	msg := fmt.Sprintf("No symbol named %q in %s.", name, fpath)
	if t.outsideFn != nil && t.outsideFn(fpath) != "" {
		msg += " (This file is outside the workspace; neither the language server nor the topology index covers it — use read_file with a line range instead.)"
	}
	return msg
}

func resolveReadSymbolPaths(path string) (fpath, uri string) {
	return strings.TrimPrefix(path, "file://"), toFileURI(path)
}

func (t *ReadSymbol) fetchReadSymbolSymbols(ctx context.Context, uri string) ([]protocol.DocumentSymbol, error) {
	key := uri + ":docSymbols"
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.([]protocol.DocumentSymbol), nil
		}
	}
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil, lspTimeoutErr("read_symbol", t.timeout, err)
	}
	if t.cache != nil {
		t.cache.Set(key, syms, t.ttl)
	}
	return syms, nil
}

func (t *ReadSymbol) formatReadSymbolResult(fpath, name string, matches []protocol.DocumentSymbol) (string, error) {
	info, err := os.Stat(fpath)
	if err != nil {
		return "", fmt.Errorf("read_symbol: %w", err)
	}
	mtime := info.ModTime()
	t.tracker.Record(fpath, mtime)
	sha, err := fileSHA256(fpath)
	if err != nil {
		slog.Warn("read_symbol: computing sha256", "path", fpath, "err", err)
	}

	var sb strings.Builder
	mtimeStr := mtime.Format(time.RFC3339Nano)
	if sha != "" {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s\n", mtimeStr, sha)
	} else {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s\n", mtimeStr)
	}
	// For clients whose native Edit tool conflicts with plumb's read-state
	// tracking, point at edit_file the moment the agent has the symbol body.
	outsideLabel := t.outsideLabel(fpath)
	// Suppress the edit-lane hint for out-of-workspace reads (not editable).
	if outsideLabel == "" && clientHasNativeEditConflict(t.clientNameFn) {
		sb.WriteString(nativeEditReadHint(mtimeStr))
	}
	if outsideLabel != "" {
		fmt.Fprintf(&sb, "# plumb-note: read-only — outside the workspace (%s); not editable\n", outsideLabel)
	}
	if len(matches) > 1 {
		fmt.Fprintf(&sb, "# %d matches for %q\n", len(matches), name)
	}

	for i, sym := range matches {
		start := int(sym.Range.Start.Line) + 1
		end := int(sym.Range.End.Line) + 1
		if len(matches) > 1 {
			fmt.Fprintf(&sb, "\n## %s (%s) lines %d–%d\n\n", sym.Name, symbolKindName(sym.Kind), start, end)
		} else {
			fmt.Fprintf(&sb, "# symbol: %s (%s) lines %d–%d\n\n", sym.Name, symbolKindName(sym.Kind), start, end)
		}
		f, ferr := os.Open(fpath)
		if ferr != nil {
			fmt.Fprintf(&sb, "(error reading lines: %v)\n", ferr)
			continue
		}
		src, rerr := readContentMaybeRanged(f, &start, &end)
		f.Close()
		if rerr != nil {
			fmt.Fprintf(&sb, "(error reading lines: %v)\n", rerr)
			continue
		}
		sb.WriteString(src)
		if i < len(matches)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String(), nil
}
