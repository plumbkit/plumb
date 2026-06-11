# cleanroom.Dockerfile — proves a fresh Linux box with NO toolchain can install
# and run plumb end-to-end.
#
# The GitHub ubuntu-latest runner ships with Go, Node and build tools, so CI
# proves the *tests* pass but never that a *fresh* machine can install and run
# plumb. This multi-stage build compiles the binary in a builder stage, then
# copies only the binary into a slim Debian runtime carrying bash + python3 and
# nothing else — no Go, no Node. The runtime drives the self-contained
# two-agents MCP e2e (docs/demos/two-agents-one-file.sh) against the installed
# binary; its exit code is the verdict. This is the automatable form of the
# launch-readiness "clean-VM verification" before tagging.
#
# Build context is the repo root (the builder needs the full source). arm64 by
# default; TARGETARCH follows DOCKER_PLATFORM from the make target. Keep the Go
# version in step with go.mod (currently `go 1.26`).

# ── builder: compile a pure-Go (CGO-off) Linux binary, version-stamped exactly
#    like the Makefile's build target. ─────────────────────────────────────────
FROM golang:1.26-bookworm AS builder
ARG VERSION=docker
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-arm64} \
    go build -ldflags "-X github.com/plumbkit/plumb/internal/cli.Version=${VERSION}" \
    -o /out/plumb ./cmd/plumb

# ── runtime: a clean Debian with no toolchain — only what the e2e driver needs.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/plumb /usr/local/bin/plumb
COPY docs/demos/two-agents-one-file.sh /opt/two-agents-one-file.sh
ENV PLUMB_BIN=/usr/local/bin/plumb
# The demo isolates HOME/XDG, drives two `plumb serve` MCP sessions, asserts the
# stale-write refusal, and self-cleans. A packaging/runtime regression exits
# non-zero and fails the container.
CMD ["bash", "/opt/two-agents-one-file.sh"]
