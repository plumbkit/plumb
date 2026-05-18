// Package jdtls is the plumb adapter for Eclipse JDT Language Server (jdtls),
// the Java language server maintained by the Eclipse Foundation.
//
// Validation status: experimental — unit-tested with mocked transport.
// No integration test against a real jdtls binary exists yet.
// To promote to validated, add integration tests in this package that spawn a
// real jdtls binary against testdata/java-fixture/ and update this comment.
//
// # Installation
//
// macOS: brew install jdtls
// Requires Java 17 or later. With SDKMAN: sdk install java 21.0.x-tem
//
// # Workspace model
//
// jdtls expects rootUri pointing to the project root (the directory containing
// pom.xml, build.gradle, or build.gradle.kts). Unlike gopls and pyright, jdtls
// requires a -data <dir> argument pointing to an Eclipse workspace storage
// directory. The plumb pool computes a per-root data directory automatically
// (see internal/cli/pool.go argsFor).
//
// # Init options
//
// InitializationOptions is kept minimal. jdtls reads project configuration
// from pom.xml or build.gradle. The Java home can be passed via
// jdtlsInitOptions.Settings.Java.Home when JAVA_HOME is not set in the
// environment; plumb populates this from JAVA_HOME if set.
//
// # Sync mode
//
// jdtls supports both full (SyncFull) and incremental (SyncIncremental) document
// sync. Plumb currently uses full-document sync for all adapters.
package jdtls
