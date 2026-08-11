//go:build integration

package kotlin_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// These tests are the promotion gate for the Kotlin adapter: they run against a
// real kotlin-lsp binary on a real, resolvable Gradle project. Both preconditions
// SKIP rather than fail, because neither is under this repository's control —
// but when they run, they are what the "validated" tier means.
//
// Diagnostics are asserted through the PULL model. kotlin-lsp advertises
// diagnosticProvider and never sends publishDiagnostics — measured at 0
// notifications in 75 s on a file with two genuine errors, with the client
// capability advertised — so a push assertion here could never pass, however
// healthy the server. See the package doc.

// requireKotlinLSP skips unless kotlin-lsp is on PATH, and returns its path.
func requireKotlinLSP(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("kotlin-lsp")
	if err != nil {
		t.Skip("kotlin-lsp not found on PATH — install with: brew install --cask kotlin-lsp " +
			"(on macOS also run: xattr -dr com.apple.quarantine /opt/homebrew/Caskroom/kotlin-lsp, " +
			"or Gatekeeper deletes the unsigned binary on first run)")
	}
	return p
}

// gradleFixture builds a genuinely resolvable Gradle project in a temp dir and
// returns its root.
//
// "Resolvable" is the whole point, and it is why this is generated rather than
// checked in. kotlin-lsp derives its classpath from the build, so a directory of
// .kt files with a build script that never resolves yields no symbols and no
// diagnostics — the server looks broken when the fixture is. The previous
// checked-in fixture was exactly that (a six-line build.gradle.kts, no settings
// file, no wrapper, no dependencies) and is the likeliest reason the old Kotlin
// adapter failed validation for months.
//
// Making it resolvable offline would mean vendoring the Kotlin compiler plugin
// and stdlib — hundreds of megabytes — so the build is left to resolve from the
// network, and the test SKIPS when Gradle is absent or the build does not
// succeed. A skip is the honest outcome: without a resolved classpath the run
// would prove nothing about the adapter either way.
func gradleFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("gradle"); err != nil {
		t.Skip("gradle not found on PATH — needed to build a resolvable Kotlin project")
	}
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.gradle.kts", `rootProject.name = "plumbfixture"`+"\n")
	// No jvmToolchain(N): pinning one fails unless that exact JDK is installed,
	// so the fixture uses whichever JDK Gradle already runs on.
	write("build.gradle.kts", `plugins { kotlin("jvm") version "2.1.0" }
repositories { mavenCentral() }
dependencies { implementation(kotlin("stdlib")) }
`)
	write("src/main/kotlin/Greeter.kt", `package fixture

class Greeter(private val name: String) {
    fun greet(): String = "Hello, $name!"
}
`)

	// Resolve BEFORE the broken file exists: a failing build leaves the classpath
	// unresolved, which is the very condition that makes a negative result
	// meaningless.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "gradle", "build", "--console=plain", "-q")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("gradle build did not succeed, so the classpath is unresolved and no "+
			"diagnostics result would mean anything (needs network on first run): %v\n%s", err, out)
	}
	return root
}

// startKotlinLSP spawns kotlin-lsp and returns a ready adapter.
func startKotlinLSP(t *testing.T, ws string) *kotlin.Adapter {
	t.Helper()
	bin := requireKotlinLSP(t)

	// --stdio is mandatory: the server defaults to a TCP socket and ignores
	// unknown flags silently, so a wrong invocation hangs instead of failing.
	// --system-path gives this run its own cache, mirroring what argsFor passes
	// per workspace root, so a test never shares an index with the user's daemon.
	cmd := exec.Command(bin, "--stdio", "--system-path", filepath.Join(t.TempDir(), "cache"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal("start kotlin-lsp:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := kotlin.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, kotlin.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	ws := gradleFixture(t)
	ad := startKotlinLSP(t, ws)
	srcPath := filepath.Join(ws, "src", "main", "kotlin", "Greeter.kt")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// No DidOpen here on purpose: the adapter's OpenTracker must supply it.
	// Unopened, this server fails documentSymbol outright rather than returning
	// nothing, so a missing ensure-open shows up here as an error.
	uri := protocol.FileURI(srcPath)
	var syms []protocol.DocumentSymbol
	var err error
	deadline := time.After(150 * time.Second)
	for {
		syms, err = ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err == nil && len(syms) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no document symbols within deadline (err=%v, n=%d)", err, len(syms))
		case <-time.After(2 * time.Second):
		}
	}

	if !hasSymbol(syms, "Greeter") {
		t.Fatalf("symbol Greeter not found; got %v", symbolNames(syms))
	}
}

// hasSymbol reports whether name appears anywhere in the symbol tree.
func hasSymbol(syms []protocol.DocumentSymbol, name string) bool {
	for _, s := range syms {
		if s.Name == name || hasSymbol(s.Children, name) {
			return true
		}
	}
	return false
}

func symbolNames(syms []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// TestIntegration_PullDiagnostics is the promotion gate for diagnostics: it
// proves the external-write → DidChangeWatchedFiles → DidOpen → diagnostics
// pipeline reaches a real server and comes back with real errors, by whichever
// model the server advertises. For kotlin-lsp that model is pull, so the
// round-trip completes with textDocument/diagnostic rather than by waiting for a
// publishDiagnostics notification that this server never sends.
func TestIntegration_PullDiagnostics(t *testing.T) {
	ws := gradleFixture(t)
	ad := startKotlinLSP(t, ws)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	if !ad.SupportsPullDiagnostics() {
		t.Fatal("kotlin-lsp did not advertise diagnosticProvider — plumb would have no " +
			"diagnostics path at all for Kotlin, since this server does not push")
	}

	// Two unambiguous errors: an unresolved call and a return-type mismatch.
	brokenPath := filepath.Join(ws, "src", "main", "kotlin", "Broken.kt")
	brokenURI := protocol.FileURI(brokenPath)
	broken := []byte(`package fixture

class Broken {
    fun bad(): String = undefinedFunction(42)

    fun alsoBad(): Int = "not an int"
}
`)
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: brokenURI, LanguageID: "kotlin", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	// The workspace has to finish resolving before an empty report means
	// anything: an unindexed project answers instantly with no items, which is
	// indistinguishable from "no errors". Poll until errors appear or time out.
	deadline := time.After(150 * time.Second)
	for {
		report, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: brokenURI},
		})
		if err == nil && report != nil && countErrors(report.Items) > 0 {
			return // success: real diagnostics for a file plumb told it about
		}
		select {
		case <-deadline:
			t.Fatal("kotlin-lsp returned no error diagnostics for Broken.kt within deadline — " +
				"the didChangeWatchedFiles + didOpen → textDocument/diagnostic pipeline is not " +
				"reaching the server, capability negotiation is broken, or the Gradle classpath " +
				"never resolved")
		case <-time.After(3 * time.Second):
		}
	}
}

func countErrors(diags []protocol.Diagnostic) int {
	n := 0
	for _, d := range diags {
		if d.Severity == protocol.SevError {
			n++
		}
	}
	return n
}
