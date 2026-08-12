// Package kotlin is the plumb adapter for JetBrains' kotlin-lsp
// (https://github.com/Kotlin/kotlin-lsp, Apache 2.0), which is built on the
// IntelliJ platform and the Kotlin plugin.
//
// Validation status: validated (pull) — kotlin-lsp 262.9593.0, 2026-08-11,
// macOS/arm64, against a resolvable Gradle project (Gradle 8.13, Kotlin 2.1.0).
// Both gated integration tests run green against the real binary:
// TestIntegration_DocumentSymbols and TestIntegration_PullDiagnostics.
//
// # Diagnostics are pull-only
//
// This is the first plumb adapter whose server does not push diagnostics at all.
// Measured on a file with two genuine errors, with the client's
// publishDiagnostics capability advertised: zero publishDiagnostics
// notifications in 75 s, while textDocument/diagnostic answered in 0.8 s with
// full IntelliJ-quality results (severity, code RETURN_TYPE_MISMATCH, source
// "Kotlin", relatedInformation, and a FIR diagnostic class in data). It
// advertises diagnosticProvider with interFileDependencies true and
// workspaceDiagnostics false.
//
// So the adapter declares the optional document-pull surface
// (SupportsPullDiagnostics + Diagnostic), and the promotion rule in
// docs/adding-an-lsp.md is satisfied through the pull round-trip. A push
// assertion could never pass here however healthy the server, which is why the
// rule reads "by whichever model the server advertises".
//
// # What else was measured, and what it implies
//
//   - Full-document sync is accepted, despite the server advertising
//     textDocumentSync: 2 (Incremental). plumb sends the whole text in one
//     contentChanges entry with no range; driving that sequence, diagnostics
//     cleared on clean text and then returned an error present only in the text
//     sent over the wire — so the server is applying plumb's updates rather than
//     re-reading the file. No incremental-sync support is needed.
//   - documentSymbol is the ONLY method needing the document open first: it
//     fails outright on an unopened file with "no stub serializer for
//     kotlin.PACKAGE_DIRECTIVE". definition, references, hover,
//     prepareCallHierarchy and the pull diagnostics all resolve for a document
//     plumb never opened, so the ensure-open set is deliberately one method.
//   - textDocument/prepareRename has no handler ("no handler for request"),
//     even though renameProvider is advertised. Harmless today: plumb's
//     rename_symbol calls Rename directly and never prepares.
//   - Cold start is fast for an IntelliJ-derived server. On a cold
//     --system-path (n=3, small single-module project, Gradle's own dependency
//     cache already warm): initialize answered in 1.7–3.8 s, and the first real
//     diagnostics arrived at 5.7–8.0 s — a stable ~4 s after initialize. No
//     rust-analyzer-style multi-minute warmup. A cold Gradle dependency cache
//     adds its own network resolve on top, which is Gradle's cost, not the
//     server's.
//     That gap is small but it is NOT zero, and it is a trap: until the
//     workspace resolves, a pull returns instantly with zero items, which is
//     indistinguishable from "no errors". Anything asserting on diagnostics must
//     poll rather than sample once.
//
// # Operating notes
//
// The server needs a rootUri pointing at a real Gradle or Maven module and
// derives its classpath from the build, so a bare directory of .kt files
// resolves nothing. Kotlin Multiplatform is NOT supported by this server; do not
// claim it. The bundled java plugins serve JVM interop and JDK resolution —
// jdtls remains plumb's Java adapter.
//
// Install with `brew install --cask kotlin-lsp` (JetBrains also publish their
// own tap, JetBrains/utils; both resolve to the same CDN artifact). It is a
// 1.3 GB installation. On macOS the downloaded binary carries
// com.apple.quarantine and Gatekeeper DELETES it on first execution, reporting
// the lie "No such file or directory" for a file that was just there — run
// `xattr -dr com.apple.quarantine /opt/homebrew/Caskroom/kotlin-lsp` after
// installing. (Observed on macOS 27; unconfirmed on a released macOS.)
//
// plumb invokes `kotlin-lsp --stdio`, plus a per-root `--system-path` cache
// directory appended by internal/cli's argsFor and pruned by the pool janitor.
// stdio is not the default — the server otherwise listens on TCP
// 127.0.0.1:9999 — and it ignores unknown flags SILENTLY, so a wrong invocation
// presents as a hang rather than an error. `kotlin-lsp` is a deprecated shim
// that exec's `bin/intellij-server` with the same arguments and warns on stderr
// at every start; it is the default because it is the only PATH-portable name,
// and `[lsp.kotlin] command` can point at the real launcher instead.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/kotlin/
// They build their own Gradle project (see gradleFixture) and skip when gradle
// or the binary is missing.
package kotlin
