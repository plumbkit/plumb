# Web UI (`plumb web`)

Plumb ships an opt-in, loopback-only **web UI** with full parity to the terminal
UI. It is served by the running daemon and launched with `plumb web`. The five
sections mirror the TUI — **Dashboard**, **Sessions**, **Memory**, **Logs**, and
the scope-aware **Settings** editor with the theme picker — so you can watch what
your agents are doing without living in a terminal.

```sh
plumb web              # start the web UI and open the browser
plumb web --no-open    # start it and just print the URL
plumb web --port 9000  # bind a specific loopback port for this launch
```

The daemon is started automatically if it is not already running.

## What it shows

- **Dashboard** — uptime and daemon vitals (CPU, heap, goroutines), a live
  CPU/heap stream (Server-Sent Events, no polling), the activity calendar
  heatmap, the busiest tools, and the token-savings breakdown (capability vs
  efficiency).
- **Sessions** — every active connection (client, language, workspace, call
  count, health), plus per-tool latency and cost-vs-frequency charts and the
  topology graph for the active workspace.
- **Memory** — browse and read the per-workspace markdown memories, with a
  workspace switcher.
- **Logs** — a live tail of the daemon log with a text filter and pause/clear.
- **Settings** — the same scope-aware editor as the TUI: a **Global** scope plus
  one scope per active workspace, each row carrying its reload tier (live /
  next-session / restart) and an **override** badge where a workspace overrides
  the global value. The theme picker switches the palette live; the web UI's
  colours track the selected plumb theme.

## Security model

The web UI is built for safety, matching plumb's "configurable safety" ethos:

- **Loopback bind only.** The listener is always bound to `127.0.0.1` — never a
  routable address. Confirm with `lsof -nP -iTCP:<port>`.
- **Opt-in, never always-on.** The daemon binds HTTP only when `plumb web` asks
  (over the control socket). There is no standing port.
- **Per-start token.** Each `web-start` mints a fresh 256-bit token, accepted as
  a query parameter on first load and then set as an `HttpOnly`, `SameSite=Strict`
  cookie. Requests without a valid token get a `401`. The token is mirrored to
  `~/Library/Caches/plumb/plumb.web.token` (mode 0600) so a later `plumb web`
  can re-print the URL.
- **CSRF guard.** State-changing requests (the config and theme write paths) are
  refused unless their `Origin`/`Host` matches the server's own loopback
  address.
- **Bounded write surface.** Like the TUI, the only writes are **config** and
  **theme** — the web UI never edits files or runs git.

No new third-party Go dependency is used — the server is stdlib `net/http`
throughout, and live updates are Server-Sent Events (no WebSocket).

## Configuration

```toml
[web]
port = 8870   # loopback TCP port for the web UI; 127.0.0.1 only
```

`[web]` is a daemon-global setting (like `[ui]`): it is read from the global
config only and ignored in project `.plumb/config.toml`. Changing the port needs
a daemon restart to take effect. `--port` overrides it for a single launch.

## Architecture

The HTTP server is hosted **inside** the daemon process (`internal/web`), where
the live pools and stores already are — so the web UI reads fresher data than the
TUI (which reads the DBs/files directly), and config writes can trigger an
in-process `store.Reload`. The frontend is a Svelte 5 + Vite + Tailwind SPA with
ECharts + uPlot charts, `go:embed`'d into the binary so the single-binary
distribution is preserved. The palette is driven by CSS custom properties fed
from `/api/theme`.

To rebuild the frontend after editing the SPA:

```sh
make web-ui   # npm ci && npm run build in internal/web/ui → dist/
make build    # rebuild the daemon with the embedded dist/
plumb restart # new code needs a fresh daemon
```

A committed placeholder `dist/index.html` keeps a bare `go build` compiling even
without a frontend build.
