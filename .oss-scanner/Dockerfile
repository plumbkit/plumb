# .oss-scanner/Dockerfile — the build image for Anthropic's OSS Scanner.
#
# The scanner clones this repository, builds this file with the checkout as the
# build context (its `.dockerignore` removed), adds its own layer (Claude Code,
# debuggers) and then audits inside the finished image with NO network, on a
# 2-CPU / 8 GB agent machine. So everything an agent needs to build plumb and
# run its tests offline is fetched here: the Go module graph, the pinned
# toolchain, and the language servers the `//go:build integration` tier drives.
# The brief the scanner reads before it starts is .oss-scanner/threat_model.md.
# The scanner keeps a repository's .dockerignore when the Dockerfile lives in the
# repository, so Dockerfile.dockerignore beside this file replaces the root one
# (BuildKit) and keeps .git in the context.
#
# Contract (see github.com/anthropics/oss-scanner, templates/Dockerfile): the
# checkout lives at /src, the final stage keeps a package manager, and a failing
# test must not fail the build. Keep the Go version in step with go.mod
# (currently `go 1.26`, `toolchain go1.26.9`), mirroring build/docker/*.Dockerfile,
# and the language-server set in step with the `integration` job in
# .github/workflows/ci.yml.
#
# Check it the way the scanner builds it: `tools/check plumb` from a checkout of
# github.com/anthropics/oss-scanner, then run the commands in the brief.
FROM golang:1.26-bookworm

# The image ships GOTOOLCHAIN=local, which would fail the build the day go.mod's
# `go` line passes the image's Go. auto fetches that toolchain now, into the
# module cache the offline audit reads from.
ENV GOTOOLCHAIN=auto

# Node only installs the npm-published language servers below. bubblewrap is the
# Linux sandbox run_command wraps commands in; without it they run unwrapped, and
# the sandbox is part of what is being audited.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs bubblewrap \
    && rm -rf /var/lib/apt/lists/*

# The language servers CI's integration job installs. typescript@5 is pinned for
# the reason ci.yml gives: TypeScript 7 ships no tsserver, so the TS test skips.
RUN go install golang.org/x/tools/gopls@latest \
    && npm install -g pyright typescript-language-server typescript@5 vscode-langservers-extracted \
    && gopls version && pyright --version && typescript-language-server --version \
    && command -v vscode-html-language-server

# scanner contract: the checkout lives inside the image, at /src
COPY . /src
WORKDIR /src

# Everything the offline audit resolves comes from this step's caches: any
# toolchain go.mod asks for beyond the image's, and, by compiling every test
# binary with the integration tag included, the test-only module graph. It also
# warms the build cache for the 2-CPU agent.
RUN go version \
    && go mod download \
    && CGO_ENABLED=0 go build -o /usr/local/bin/plumb ./cmd/plumb \
    && go build ./... \
    && go test -count=1 -run '^$' ./... \
    && go test -tags=integration -count=1 -run '^$' ./... \
    && plumb version

# From here on a missing module fails fast instead of hanging on a dead network;
# the cached toolchain and modules still resolve.
ENV GOPROXY=off

# The fast path through the suite, so the image is known to work. A failing test
# is reported, not fatal: the scanner still gets an image to audit.
RUN mkdir -p .testcache \
    && (GOTMPDIR=/src/.testcache go test -short -timeout=15m ./internal/paths/... ./internal/tools/... \
        || echo "WARNING: go test reported failures")
