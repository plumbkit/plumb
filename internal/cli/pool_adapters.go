package cli

import (
	"crypto/sha256"
	"encoding/hex"
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

// initParamsFor builds the Initialize params for a language, negotiated for the
// requested diagnostics mode. For "push" (the default) the params are the
// adapter's push-first defaults, unchanged. For "pull" the params are opted into
// the LSP 3.17 pull model: an adapter with pull-specific initialization options
// (gopls, via lsp.PullInitializer) customises the params itself; every other
// adapter gets the generic client-capability swap. ad is the constructed adapter
// instance the type assertion targets. lspCfg supplies the language's optional
// free-form InitializationOptions, overlaid verbatim by defaultInitParamsFor.
func initParamsFor(ad lsp.Client, language, rootURI, requested string, lspCfg config.LSPConfig) protocol.InitializeParams {
	params := defaultInitParamsFor(language, rootURI, lspCfg)
	if requested != diagModePull {
		return params
	}
	if pi, ok := ad.(lsp.PullInitializer); ok {
		pi.EnablePullDiagnostics(&params)
	} else {
		params.Capabilities = protocol.ClientCapabilitiesFor(true)
	}
	return params
}

// defaultInitParamsFor returns a language's push-first default Initialize params,
// with any user-configured [lsp.<lang>] initialization_options overlaid verbatim
// onto InitializationOptions. When unconfigured the params are byte-identical to
// the adapter's own DefaultInitParams (the overlay is a no-op, so a typed default
// like gopls's is preserved untouched).
func defaultInitParamsFor(language, rootURI string, lspCfg config.LSPConfig) protocol.InitializeParams {
	params := adapterInitParams(language, rootURI)
	if len(lspCfg.InitializationOptions) > 0 {
		params.InitializationOptions = lspCfg.InitializationOptions
	}
	return params
}

// adapterInitParams dispatches to each adapter's own push-first default params.
func adapterInitParams(language, rootURI string) protocol.InitializeParams {
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

// stateDirSpec describes a language whose server keeps heavyweight per-project
// state (an index, a resolved classpath) in a directory named on the command
// line. The directory must be per-root: two projects sharing one would fight
// over the same index. It cannot live in [lsp.<lang>] args, because it is
// derived from the workspace root, which config cannot see.
type stateDirSpec struct {
	flag   string // CLI flag introducing the directory
	subdir string // cache subdirectory holding the per-root dirs
}

// serverStateDirs is the whole set of such languages. jdtls wants an Eclipse
// workspace; kotlin-lsp wants an IntelliJ system path (caches and indexes for a
// 1.3 GB server distribution). Adding a language here is all that is needed —
// argsFor passes the flag and the janitor prunes the directory.
var serverStateDirs = map[string]stateDirSpec{
	"java":   {flag: "-data", subdir: "jdtls-data"},
	"kotlin": {flag: "--system-path", subdir: "kotlin-lsp-data"},
}

// argsFor returns the supervisor args for the given language and workspace root:
// lspCfg.Args verbatim, plus a per-root state directory for the languages in
// serverStateDirs.
func argsFor(language, root string, lspCfg config.LSPConfig) []string {
	spec, ok := serverStateDirs[language]
	if !ok {
		return lspCfg.Args
	}
	dataDir := serverStateDir(language, root)
	_ = os.MkdirAll(dataDir, 0o700)
	// Stamp the data dir's mtime on each cold start so pruning can treat it as a
	// reliable "last opened" signal — a server's own writes land in nested files
	// and don't update the top-level dir mtime, and MkdirAll on an existing dir
	// doesn't either.
	now := time.Now()
	_ = os.Chtimes(dataDir, now, now)
	out := make([]string, len(lspCfg.Args), len(lspCfg.Args)+2)
	copy(out, lspCfg.Args)
	return append(out, spec.flag, dataDir)
}

// serverStateDir returns the per-workspace state directory for a language in
// serverStateDirs, or "" for any other. The directory name is a hash of the
// workspace root, so each project gets isolated state.
func serverStateDir(language, root string) string {
	spec, ok := serverStateDirs[language]
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(config.CacheDir(), spec.subdir, hex.EncodeToString(sum[:8]))
}
