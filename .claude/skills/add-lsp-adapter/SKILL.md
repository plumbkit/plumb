---
name: add-lsp-adapter
description: Add or validate a language-server adapter in plumb — the base.Adapter shape, the promotion hazard, the conformance and error-contract harnesses, and the experimental-to-validated promotion rule. Use when adding a language or debugging an adapter's diagnostics round-trip.
---

# Adding an LSP adapter to plumb

An adapter embeds `*base.Adapter` (`internal/lsp/adapters/base`), which supplies all 23
`lsp.Client` methods, and shadows only what its server does differently. `rust` is the
minimal worked example (46 lines); full guide in `docs/adding-an-lsp.md`.

## The 5 steps

1. Create `internal/lsp/adapters/<name>/` with a `doc.go` stating the adapter's validation
   status (Experimental or Validated — see the promotion rule below).
2. Write `adapter.go`: `type Adapter struct{ *base.Adapter }`, `var _ lsp.Client =
   (*Adapter)(nil)`, `func New(conn jsonrpc.Caller) *Adapter { return &Adapter{Adapter:
   base.New(conn, "<binary-name>")} }`, and `DefaultInitParams` — the one genuinely
   per-server function. Do NOT hand-write the 23 methods.
3. Shadow only the methods your server does differently, calling through with
   `a.Adapter.X(ctx, params)`. Escape hatches are package-level FUNCTIONS —
   `base.Call[T]`, `base.CallPtr[T]`, `base.CallRaw`, `base.Notify`, `base.Wrap`. **Never
   add an exported method to `base.Adapter`**: Go promotes it into all nine adapters and
   `internal/cli` resolves optional capabilities structurally, so one stray method silently
   opts every server into a capability it does not have. An adapter-only capability
   (e.g. pull diagnostics) is declared in the adapter's own package.
4. `base.New` installs BOTH transport handlers (notification fan-out + server-request
   handler); `dispatch` and `handleServerRequest` are unexported, so there is nothing to
   register and nothing to forget. Server-specific work left to you: init options,
   workspace model, and — for a server that answers only for open documents — a
   `base.OpenTracker` held as a NAMED field (never embedded) with an `Ensure` call in each
   affected method and a `Refresh` in `DidChangeWatchedFiles`. The set of methods needing
   it differs per server; do not copy another adapter's.
5. Test: unit tests with `internal/lsp/jsonrpc/mock.go` covering what this adapter adds
   (not the inherited base); `conformance.RunConformance` for the protocol contract; and
   `conformance.RunErrorContract` for the `"<server> <label>: <cause>"` strings. Lazy-open
   adapters also call `conformance.RunLazyOpenErrorContract`.

## The experimental → validated promotion rule

An adapter stays **Experimental** until the `DidChangeWatchedFiles`+`DidOpen` →
`publishDiagnostics` round-trip runs green against a **real server binary**, in an
integration test gated `//go:build integration` — mock-transport unit tests passing is not
enough to promote. Update the adapter's `doc.go` and the validation table in `AGENTS.md`
("## Adapter validation status") when a real-binary retest goes green.

## Recurring gotchas (from the adapter validation table)

- **Some servers resolve nothing for an unopened document.** zls needs the file opened via
  `didOpen` before any per-document query (documentSymbol, definition, references, hover,
  hierarchy prepares) resolves. Fix: `base.OpenTracker` — open lazily on first query, close
  on a watched-file change. sourcekit-lsp and vscode-html-language-server are the other two.
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

Full step-by-step guide, including the promotion hazard and the three worked examples
(rust / typescript / swift): `docs/adding-an-lsp.md`.
