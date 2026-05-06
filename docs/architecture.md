# Architecture

> Full diagram and explanation to be written in Step 9.

## Layers

```
┌─────────────────────────────────────────────────────────┐
│  Presentation   internal/tui   internal/cli             │
├─────────────────────────────────────────────────────────┤
│  Application    internal/tools   internal/cache         │
├─────────────────────────────────────────────────────────┤
│  Domain         internal/domain   internal/workspace    │
├─────────────────────────────────────────────────────────┤
│  Transport      internal/mcp   internal/lsp             │
└─────────────────────────────────────────────────────────┘
```

Lower layers must not import higher layers.
