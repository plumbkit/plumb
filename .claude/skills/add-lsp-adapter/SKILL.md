---
name: add-lsp-adapter
description: Add or validate a language-server adapter in plumb — LSPClient interface, capability negotiation, server-request handling, DidChangeWatchedFiles wire tests, and the experimental-to-validated promotion rule. Use when adding a language or debugging an adapter's diagnostics round-trip.
---

# Adding an LSP adapter to plumb

Pyright is the worked example; full guide in `docs/adding-an-lsp.md`.

## The 5 steps

1. Create `internal/lsp/adapters/<name>/` with a `doc.go` stating the adapter's validation
   status (Experimental or Validated — see the promotion rule below).
2. Implement every `LSPClient` method (`internal/lsp/client.go`), including
   `DidChangeWatchedFiles` — the LSP-correct primitive for external file changes. No
   per-adapter extension methods; the interface is the contract every tool codes against.
3. Register `conn.SetRequestHandler(a.handleServerRequest)` to answer
   `client/registerCapability` / `client/unregisterCapability`; without it the server may
   stall waiting for a response it never gets.
4. Implement initialisation: capability negotiation based on
   `protocol.DefaultClientCapabilities()`, workspace model, init options.
5. Unit-test with `internal/lsp/jsonrpc/mock.go`; cover the `DidChangeWatchedFiles` wire
   format specifically (gopls and pyright have explicit tests to pattern-match against).

## The experimental → validated promotion rule

An adapter stays **Experimental** until the `DidChangeWatchedFiles`+`DidOpen` →
`publishDiagnostics` round-trip runs green against a **real server binary**, in an
integration test gated `//go:build integration` — mock-transport unit tests passing is not
enough to promote. Update the adapter's `doc.go` and the validation table in `AGENTS.md`
("## Adapter validation status") when a real-binary retest goes green.

## Recurring gotchas (from the adapter validation table)

- **Some servers resolve nothing for an unopened document.** zls needs the file opened via
  `didOpen` before any per-document query (documentSymbol, definition, references, hover,
  hierarchy prepares) resolves. Fix: open lazily on first query, close on a watched-file
  change.
- **Some servers publish nothing unless the client advertises
  `textDocument.publishDiagnostics`.** typescript-language-server does not implement pull
  diagnostics (`textDocument/diagnostic` returns -32601) — it silently produces no
  diagnostics at all if that client capability isn't declared in
  `DefaultClientCapabilities()`. If a new adapter's diagnostics round-trip test fails with
  no published diagnostics and no error, check this first before chasing a pull-diagnostics
  theory.
- **Some servers need a real project layout, not a bare temp workspace**, to publish
  diagnostics at all (kotlin-language-server needs a real Gradle/Maven project) — a
  same-symptom failure with a different root cause from the two gotchas above.

## Level-3 reference

Full step-by-step guide, including the Pyright worked example in detail: `docs/adding-an-lsp.md`.
