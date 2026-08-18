# Dockerfile — the canonical container for plumb as a stdio MCP server.
#
# `plumb serve` is a self-contained stdio server: on first launch it spawns the
# background daemon (the same binary, `plumb daemon`) and then proxies MCP
# JSON-RPC over stdin/stdout. This image compiles the pure-Go binary once, then
# runs it in a slim runtime under an isolated HOME/XDG so the daemon's socket,
# cache, and state files land inside the container. That is exactly what a
# curated-listing checker needs: the server starts and answers initialize and
# tools/list with no toolchain and no language server installed (the daemon
# warns and still serves topology, filesystem, git, and memory tools).
#
# Build context is the repo root. Keep the Go version in step with go.mod
# (currently `go 1.26`), mirroring build/docker/cleanroom.Dockerfile.

# ── builder: compile a pure-Go (CGO-off) Linux binary, version-stamped like the
#    Makefile's build target. ─────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder
ARG VERSION=docker
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags "-X github.com/plumbkit/plumb/internal/cli.Version=${VERSION}" \
    -o /out/plumb ./cmd/plumb

# ── runtime: slim Debian, non-root user, isolated HOME. ──────────────────────
FROM debian:bookworm-slim AS runtime
RUN useradd --create-home --uid 10001 plumb
COPY --from=builder /out/plumb /usr/local/bin/plumb
USER plumb
WORKDIR /workspace
ENV HOME=/home/plumb
ENTRYPOINT ["plumb"]
CMD ["serve"]
