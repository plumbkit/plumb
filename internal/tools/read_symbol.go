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
    "name": {
      "type": "string",
      "description": "Exact symbol name. Accepts plain name (e.g. \"handleConn\") or dotted ReceiverType.MethodName form (e.g. \"Model.renderDashboard\")."
    }
  },
  "required": ["path", "name"]
}`)

// ReadSymbol returns the source body of a named symbol in one call,
// collapsing the list_symbols + read_file two-round-trip pattern.
// When multiple symbols share the name, all matches are returned.
// The mtime header matches read_file so callers can pass it as expected_mtime.
//
// Concurrency: Execute is safe for concurrent use.
type ReadSymbol struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	tracker *ReadTracker
	topo    topologyStoreFn
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
	Name string `json:"name"`
}

func (t *ReadSymbol) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseReadSymbolArgs(raw)
	if err != nil {
		return "", err
	}
	fpath, uri := resolveReadSymbolPaths(a.Path)
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
		return fmt.Sprintf("No symbol named %q in %s.", a.Name, fpath), nil
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
		return a, fmt.Errorf("read_symbol: path is required")
	}
	if a.Name == "" {
		return a, fmt.Errorf("read_symbol: name is required")
	}
	return a, nil
}

func resolveReadSymbolPaths(path string) (fpath, uri string) {
	fpath = strings.TrimPrefix(path, "file://")
	uri = path
	if !strings.HasPrefix(uri, "file://") {
		uri = "file://" + uri
	}
	return fpath, uri
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
	if sha != "" {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s\n", mtime.Format(time.RFC3339Nano), sha)
	} else {
		fmt.Fprintf(&sb, "# plumb-read mtime=%s\n", mtime.Format(time.RFC3339Nano))
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
