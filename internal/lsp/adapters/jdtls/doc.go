// Package jdtls is the plumb adapter for Eclipse JDT Language Server (jdtls),
// the Java language server maintained by the Eclipse Foundation.
//
// Validation status: experimental — unit-tested with mocked transport;
// integration test exists (integration_test.go, gated with //go:build integration)
// but has not yet run in CI because no CI runner installs jdtls.
// To promote to validated, run the integration test against a real jdtls binary
// and add a CI step that installs jdtls; then update this comment.
//
// # Installation
//
// Install jdtls and ensure it is on PATH. Requires Java 21 or later.
//
//	macOS (Homebrew):  brew install jdtls
//	SDKMAN (Java):     sdk install java 21-tem
//	Other platforms:   download from https://github.com/eclipse-jdtls/eclipse.jdt.ls/releases
//	                   and place the launcher script on PATH as "jdtls"
//
// # Workspace model
//
// jdtls expects rootUri pointing to the project root (the directory containing
// pom.xml, build.gradle, or build.gradle.kts). Unlike gopls and pyright, jdtls
// requires a -data <dir> argument pointing to an Eclipse workspace storage
// directory. The plumb pool computes a per-root data directory automatically
// (see internal/cli/pool.go argsFor); no manual configuration is needed.
//
// # Init options
//
// InitializationOptions is kept minimal. jdtls reads project configuration
// from pom.xml or build.gradle. The Java home is populated from JAVA_HOME
// when set in the daemon's environment; leave it unset to let jdtls discover
// the JDK via its own detection logic (recommended when using SDKMAN).
// Per-language environment overrides can also be set via [lsp.java].env in
// config.toml, which are merged on top of the daemon's environment.
//
// # Sync mode
//
// jdtls supports both full (SyncFull) and incremental (SyncIncremental)
// document sync. Plumb currently uses full-document sync for all adapters.
package jdtls
