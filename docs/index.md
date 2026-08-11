# Plumb Documentation

Plumb is an [MCP](https://modelcontextprotocol.io) server that gives AI
assistants real IDE intelligence — go-to-definition, find-references, rename,
diagnostics, atomic edits, and semantic refactors — backed by the same
[LSP](https://microsoft.github.io/language-server-protocol/) language servers
your editor uses, plus an optional SQLite/FTS5 topology index.

New here? Start with the [README](../README.md), then
[Getting Started](getting-started.md).

## Get started

- [**Getting Started**](getting-started.md) — install, connect your assistant, initialise a project, first session.

## Reference

- [**CLI Reference**](cli-reference.md) — every command, subcommand, and flag.
- [**Configuration**](configuration.md) — all config sections, environment variables, and a sample `config.toml`.
- [**Tools (MCP API)**](tools.md) — the 58 tools, with inputs and conventions.

## Concepts

- [**Architecture**](architecture.md) — layers, the daemon/proxy model, data flow, and persistence (with diagrams).
- [**Topology**](topology.md) — the optional semantic index and the dual-engine (Topology + LSP) model.
- [**Token Efficiency**](token-efficiency.md) — how plumb keeps assistant context lean.
- [**Threat model**](threat-model.md) — assets, trust boundaries, abuse cases, and what plumb deliberately does *not* defend against.

## Contributing

- [**Contributing**](contributing.md) — build/test/lint workflow, code style, commit conventions.
- [**Adding an LSP adapter**](adding-an-lsp.md) — the worked example for a new language.
- [`AGENTS.md`](../AGENTS.md) — the canonical brief for contributors and AI agents: the rules, plus pointers into the pages above for reference detail.

## Help

- [**Troubleshooting**](troubleshooting.md) — common failures and fixes; start with `plumb doctor`.
- [`CHANGELOG.md`](../CHANGELOG.md) — release history.
