package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	htmlls "github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/adapters/jdtls"
	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/adapters/pyright"
	"github.com/plumbkit/plumb/internal/lsp/adapters/rust"
	"github.com/plumbkit/plumb/internal/lsp/adapters/swift"
	tsls "github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// newAdapter constructs the right adapter for a language.
func newAdapter(language string, conn *jsonrpc.Conn) (lsp.Client, error) {
	switch language {
	case "go":
		return gopls.New(conn), nil
	case "java":
		return jdtls.New(conn), nil
	case "python":
		return pyright.New(conn), nil
	case "rust":
		return rust.New(conn), nil
	case "swift":
		return swift.New(conn), nil
	case "zig":
		return zig.New(conn), nil
	case "typescript":
		return tsls.New(conn), nil
	case "kotlin":
		return kotlin.New(conn), nil
	case "html":
		return htmlls.New(conn), nil
	default:
		return nil, fmt.Errorf("no adapter registered for language %q", language)
	}
}

// initParamsFor builds the Initialize params for a language.
func initParamsFor(language, rootURI string) protocol.InitializeParams {
	switch language {
	case "java":
		return jdtls.DefaultInitParams(rootURI)
	case "python":
		return pyright.DefaultInitParams(rootURI)
	case "rust":
		return rust.DefaultInitParams(rootURI)
	case "swift":
		return swift.DefaultInitParams(rootURI)
	case "zig":
		return zig.DefaultInitParams(rootURI)
	case "typescript":
		return tsls.DefaultInitParams(rootURI)
	case "kotlin":
		return kotlin.DefaultInitParams(rootURI)
	case "html":
		return htmlls.DefaultInitParams(rootURI)
	default:
		return gopls.DefaultInitParams(rootURI)
	}
}

// argsFor returns the supervisor args for the given language and workspace root.
// For most languages this is lspCfg.Args verbatim. Java is special: jdtls
// requires a -data <dir> argument pointing to an Eclipse workspace storage
// directory. Using a per-root directory prevents classpath conflicts when
// multiple Java projects are open simultaneously.
func argsFor(language, root string, lspCfg config.LSPConfig) []string {
	if language != "java" {
		return lspCfg.Args
	}
	dataDir := jdtlsDataDir(root)
	_ = os.MkdirAll(dataDir, 0o700)
	// Stamp the data dir's mtime on each cold start so pruneJdtlsCache can treat
	// it as a reliable "last opened" signal — jdtls's own writes land in nested
	// files and don't update the top-level dir mtime, and MkdirAll on an existing
	// dir doesn't either.
	now := time.Now()
	_ = os.Chtimes(dataDir, now, now)
	out := make([]string, len(lspCfg.Args), len(lspCfg.Args)+2)
	copy(out, lspCfg.Args)
	return append(out, "-data", dataDir)
}

// jdtlsDataDir returns a per-workspace Eclipse workspace data directory for
// jdtls. The directory name is derived from a hash of the workspace root so
// each project gets isolated Eclipse state.
func jdtlsDataDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(config.CacheDir(), "jdtls-data", fmt.Sprintf("%x", sum[:8]))
}
