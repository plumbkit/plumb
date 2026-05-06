# plumb

Plumb is an MCP (Model Context Protocol) server that exposes LSP (Language Server Protocol) capabilities to LLMs. Instead of dumping raw source files into context, Plumb lets LLMs query codebases through structured semantic tools — finding symbols, jumping to definitions, listing references, and applying targeted edits — saving tokens and improving accuracy.

The server is consumed by MCP clients such as Claude Desktop. It ships with a validated Go adapter (gopls) and an experimental Python adapter (pyright), a caching layer that memoises results within a session, composite high-level tools that bundle multiple LSP queries into a single curated response, and a small CLI for setup and monitoring.

## Quick start

```sh
# Install
go install github.com/golimpio/plumb/cmd/plumb@latest

# Wire up Claude Desktop
plumb setup claude-desktop

# Serve (stdio, for MCP clients)
plumb serve

# Monitor a live session
plumb status
```

## Build from source

```sh
make build      # produces ./plumb
make test-race  # full test suite with race detector
make lint       # golangci-lint
```

Requires Go 1.26+. gopls must be on `$PATH` for the Go adapter integration tests.
