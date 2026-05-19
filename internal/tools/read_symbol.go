package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	tracker *ReadTracker
}

func NewReadSymbol(client lsp.Client, c *cache.Cache, ttl time.Duration, tracker *ReadTracker) *ReadSymbol {
	return &ReadSymbol{client: client, cache: c, ttl: ttl, tracker: tracker}
}

func (t *ReadSymbol) Name() string                 { return "read_symbol" }
func (t *ReadSymbol) InputSchema() json.RawMessage { return readSymbolSchema }
func (t *ReadSymbol) Description() string {
	return "Read the source body of a named symbol (function, method, type) in one call — " +
		"no native Claude Code equivalent for this LSP-backed lookup. " +
		"Accepts plain name or dotted ReceiverType.MethodName form. " +
		"Returns all matches when the name is ambiguous. " +
		"Prefer this over a list_symbols + read_file pair for targeted symbol reads."
}

type readSymbolArgs struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (t *ReadSymbol) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a readSymbolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("read_symbol: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("read_symbol: path is required")
	}
	if a.Name == "" {
		return "", fmt.Errorf("read_symbol: name is required")
	}

	fpath := strings.TrimPrefix(a.Path, "file://")
	uri := a.Path
	if !strings.HasPrefix(uri, "file://") {
		uri = "file://" + uri
	}

	key := uri + ":docSymbols"
	var syms []protocol.DocumentSymbol
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			syms = v.([]protocol.DocumentSymbol)
		}
	}
	if syms == nil {
		var err error
		syms, err = t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err != nil {
			return "", fmt.Errorf("read_symbol: %w", err)
		}
		if t.cache != nil {
			t.cache.Set(key, syms, t.ttl)
		}
	}

	matches := resolveSymbolsByName(syms, a.Name)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol named %q in %s.", a.Name, fpath), nil
	}

	info, err := os.Stat(fpath)
	if err != nil {
		return "", fmt.Errorf("read_symbol: %w", err)
	}
	mtime := info.ModTime()
	t.tracker.Record(fpath, mtime)

	sha, _ := fileSHA256(fpath)

	var sb strings.Builder
	fmt.Fprintf(&sb, "# plumb-read mtime=%s sha256=%s\n", mtime.Format(time.RFC3339Nano), sha)
	if len(matches) > 1 {
		fmt.Fprintf(&sb, "# %d matches for %q\n", len(matches), a.Name)
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
