# Adding an LSP Adapter

> Full step-by-step guide with gopls and pyright as worked examples to be written in Step 9.

## Validation levels

- **Validated**: adapter spawns a real binary in integration tests. Only gopls in v0.
- **Experimental**: real Go code, unit-tested with a mocked transport, no real binary in CI.

## Quick reference

See `AGENTS.md` → "How to add an LSP adapter" for the step-by-step checklist.
