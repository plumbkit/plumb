//go:build integration

package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/adapters/swift"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/xcodebsp"
)

func requireXcodeIntegration(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("real Xcode integration requires macOS")
	}
	for _, command := range []string{"xcodebuild", "xcode-build-server", "sourcekit-lsp"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is not installed", command)
		}
	}
}

func copyXcodeFixture(t *testing.T) string {
	t.Helper()
	source := filepath.Join(repoRootForCLI(t), "testdata", "xcode-fixture")
	root := t.TempDir()
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func repoRootForCLI(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repository root")
		}
		dir = parent
	}
}

type sourceKitFixture struct {
	cmd  *exec.Cmd
	conn *jsonrpc.Conn
	ad   *swift.Adapter
}

func startSourceKitFixture(t *testing.T, root string) *sourceKitFixture {
	t.Helper()
	bin, err := exec.LookPath("sourcekit-lsp")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = root
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	conn := jsonrpc.NewConn(stdout, stdin)
	ad := swift.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := ad.Initialize(ctx, swift.DefaultInitParams(protocol.FileURI(root))); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("initialize SourceKit-LSP: %v\n%s", err, stderr.String())
	}
	if err := ad.Initialized(ctx); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("initialized SourceKit-LSP: %v\n%s", err, stderr.String())
	}
	return &sourceKitFixture{cmd: cmd, conn: conn, ad: ad}
}

func (p *sourceKitFixture) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.ad.Shutdown(ctx)
	_ = p.ad.Exit(ctx)
	_ = p.conn.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}

func runXcodeArgv(t *testing.T, root string, timeout time.Duration, argv ...string) xcodebsp.ExecResult {
	t.Helper()
	result, err := (xcodeArgvRunner{}).Run(context.Background(), root, argv, timeout)
	if err != nil || result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("%v failed: err=%v exit=%d timeout=%v\nstdout:\n%s\nstderr:\n%s", argv, err, result.ExitCode, result.TimedOut, result.Stdout, result.Stderr)
	}
	return result
}

func TestIntegrationXcodeColdConfigBuildRestartAndSemantics(t *testing.T) {
	requireXcodeIntegration(t)
	root := copyXcodeFixture(t)
	project := filepath.Join(root, "PlumbXcodeFixture.xcodeproj")
	source := filepath.Join(root, "Sources", "main.swift")

	cold := startSourceKitFixture(t, root)
	coldSymbols, _ := cold.ad.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Greeter"})
	if len(coldSymbols) != 0 {
		cold.stop()
		t.Fatalf("cold SourceKit-LSP unexpectedly returned %d Greeter symbol(s) without buildServer.json", len(coldSymbols))
	}

	status := xcodebsp.Configure(context.Background(), xcodebsp.ConfigureRequest{
		Root: root, Enabled: true, Trusted: true, Timeout: 2 * time.Minute,
	}, xcodeArgvRunner{})
	if status.State != xcodebsp.StateConfiguredNeedsBuildData {
		cold.stop()
		t.Fatalf("configuration state = %#v", status)
	}
	if _, err := os.Stat(filepath.Join(root, ".compile")); !os.IsNotExist(err) {
		cold.stop()
		t.Fatalf("configuration must not produce build data; .compile stat err = %v", err)
	}
	stillCold, _ := cold.ad.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Greeter"})
	if len(stillCold) != 0 {
		cold.stop()
		t.Fatalf("running SourceKit-LSP hot-discovered buildServer.json; got %d symbol(s)", len(stillCold))
	}
	cold.stop()

	derivedData := filepath.Join(t.TempDir(), "DerivedData")
	runXcodeArgv(t, root, 5*time.Minute,
		"xcodebuild", "-project", project, "-scheme", "PlumbXcodeFixture",
		"-configuration", "Debug", "-derivedDataPath", derivedData,
		"-destination", "platform=macOS", "CODE_SIGNING_ALLOWED=NO", "build",
	)
	runXcodeArgv(t, root, 2*time.Minute,
		"xcode-build-server", "config", "--build_root", derivedData,
		"-scheme", "PlumbXcodeFixture", "-project", project,
	)
	runXcodeArgv(t, root, 2*time.Minute,
		"xcode-build-server", "parse", "-s", derivedData, "-o", ".compile",
	)
	if info, err := os.Stat(filepath.Join(root, ".compile")); err != nil || info.Size() == 0 {
		t.Fatalf("parsed compilation data missing or empty: info=%v err=%v", info, err)
	}

	warm := startSourceKitFixture(t, root)
	defer warm.stop()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	uri := protocol.FileURI(source)
	if err := warm.ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: "swift", Version: 1, Text: string(data)},
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(90 * time.Second)
	var symbols []protocol.SymbolInformation
	for time.Now().Before(deadline) {
		symbols, err = warm.ad.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "Greeter"})
		if err == nil && len(symbols) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if len(symbols) == 0 {
		t.Fatalf("no workspace symbols after explicit build and SourceKit-LSP restart: err=%v", err)
	}

	definitions, err := warm.ad.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 7, Character: 5},
	})
	if err != nil || len(definitions) == 0 {
		t.Fatalf("definition after restart: n=%d err=%v", len(definitions), err)
	}
	references, err := warm.ad.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 0, Character: 8},
		Context:      protocol.ReferenceContext{IncludeDeclaration: true},
	})
	if err != nil || len(references) < 2 {
		t.Fatalf("references after restart: n=%d err=%v", len(references), err)
	}
}

type countingXcodeRunner struct {
	inner xcodebsp.Runner
	mu    sync.Mutex
	calls int
}

func (r *countingXcodeRunner) Run(ctx context.Context, root string, argv []string, timeout time.Duration) (xcodebsp.ExecResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.inner.Run(ctx, root, argv, timeout)
}

func TestIntegrationXcodeConcurrentAttachSingleflight(t *testing.T) {
	requireXcodeIntegration(t)
	root := copyXcodeFixture(t)
	runner := &countingXcodeRunner{inner: xcodeArgvRunner{}}
	var restartMu sync.Mutex
	restarts := 0
	pool := &workspacePool{
		baseCtx: context.Background(),
		xcode: poolXcodeState{
			trust: staticXcodeTrust(true), runner: runner,
			restart: func(string) error {
				restartMu.Lock()
				restarts++
				restartMu.Unlock()
				return nil
			},
		},
	}
	cfg := config.XcodeConfig{AutoBuildServer: true, Timeout: config.Duration{Duration: 2 * time.Minute}}

	var callers sync.WaitGroup
	for range 2 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			pool.ensureXcodeBuildServer(root, cfg)
		}()
	}
	callers.Wait()
	waitXcodeState(t, pool, root, xcodebsp.StateWarming)

	runner.mu.Lock()
	calls := runner.calls
	runner.mu.Unlock()
	if calls != 2 {
		t.Fatalf("real runner calls = %d, want one list and one config", calls)
	}
	restartMu.Lock()
	defer restartMu.Unlock()
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1", restarts)
	}
}
