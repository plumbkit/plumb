package jdtls

import (
	"os"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// jdtlsInitOptions holds jdtls-specific initialisation options.
// jdtls reads most project config from pom.xml / build.gradle; these options
// let callers pass a Java-home override when JAVA_HOME is not set.
type jdtlsInitOptions struct {
	Settings *jdtlsSettings `json:"settings,omitempty"`
}

type jdtlsSettings struct {
	Java *jdtlsJavaSettings `json:"java,omitempty"`
}

// jdtlsJavaSettings mirrors the eclipse.jdt.ls java settings namespace.
type jdtlsJavaSettings struct {
	// Home overrides JAVA_HOME for the jdtls process. Leave empty to let jdtls
	// discover the JDK from JAVA_HOME or its own detection logic.
	Home string `json:"home,omitempty"`
}

// Adapter implements lsp.Client for Eclipse JDT Language Server (jdtls).
//
// jdtls uses the same JSON-RPC 2.0 / LSP protocol as gopls and pyright but
// requires a -data <dir> process argument (passed by the pool) and sends
// client/registerCapability requests for watched-file patterns during init,
// which the embedded base answers so DidChangeWatchedFiles events are filtered
// to the registered globs.
//
// Every lsp.Client method is inherited from base.Adapter, which labels each
// error "jdtls <label>: <cause>"; this package adds only jdtls's initialisation
// options.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "jdtls")}
}

// DefaultInitParams returns InitializeParams suitable for jdtls.
// rootURI must be a file:// URI pointing to the Java project root (the
// directory containing pom.xml or build.gradle).
//
// JAVA_HOME is read here, at call time rather than at construction, so a daemon
// whose environment changes between attaches picks the new value up.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	var opts jdtlsInitOptions
	if home := os.Getenv("JAVA_HOME"); home != "" {
		opts.Settings = &jdtlsSettings{Java: &jdtlsJavaSettings{Home: home}}
	}
	return protocol.InitializeParams{
		ProcessID:             protocol.ProcessID(),
		ClientInfo:            &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:               rootURI,
		Capabilities:          protocol.DefaultClientCapabilities(),
		InitializationOptions: opts,
	}
}
