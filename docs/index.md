# Plumb Documentation

Plumb is an [MCP](https://modelcontextprotocol.io) server that gives AI
assistants real IDE intelligence — go-to-definition, find-references, rename,
diagnostics, atomic edits, and semantic refactors — backed by the same
[LSP](https://microsoft.github.io/language-server-protocol/) language servers
your editor uses, plus an optional SQLite/FTS5 topology index — and, because
every agent on a machine shares one plumb daemon, a coordination layer that
lets several agents work one repository at once: peer awareness, an
agent-to-agent mailbox, and opt-in intents and durable knowledge handoff.

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
- [**Cross-agent sharing**](tools.md#cross-agent-sharing-collab) — how concurrent agents see each other's observed writes, message each other, and hand off findings; gating and defaults in the [`[collab]` config section](configuration.md#collab--cross-agent-sharing).
- [**Threat model**](threat-model.md) — assets, trust boundaries, abuse cases, and what plumb deliberately does *not* defend against.

## Contributing

- [**Contributing**](contributing.md) — build/test/lint workflow, code style, commit conventions.
- [**Adding an LSP adapter**](adding-an-lsp.md) — the worked example for a new language.
- [`AGENTS.md`](../AGENTS.md) — the canonical brief for contributors and AI agents: the rules, plus pointers into the pages above for reference detail.

## Help

- [**Troubleshooting**](troubleshooting.md) — common failures and fixes; start with `plumb doctor`.
- [`CHANGELOG.md`](../CHANGELOG.md) — release history.
