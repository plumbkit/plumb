# integration.Dockerfile — local reproduction of the CI Linux integration job.
#
# plumb is developed on macOS only; this image lets a macOS developer run the
# real-binary LSP integration suite (gopls + pyright) on Linux locally, before
# pushing, instead of debugging a red ubuntu CI job push-by-push. It mirrors the
# `integration` job in .github/workflows/ci.yml (gopls + pyright on PATH, then
# `make integration-test`).
#
# The repo is bind-mounted at run time (see `make docker-integration`), so this
# image carries only the toolchain, never the source — it always reflects the
# working tree. Build for the host architecture (arm64 on Apple Silicon); the
# binary is pure-Go (CGO_ENABLED=0), so arch rarely matters, but pass
# DOCKER_PLATFORM=linux/amd64 to the make target for amd64 fidelity (emulated).
#
# Keep the Go version in step with go.mod (currently `go 1.26`).
FROM golang:1.26-bookworm

# Node is needed only to install pyright (the Python language server).
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Real language-server binaries — the whole point of the integration tier.
RUN go install golang.org/x/tools/gopls@latest \
    && npm install -g pyright

ENV PATH="/go/bin:${PATH}"

# Sanity check at build time so a broken toolchain fails the image, not the run.
RUN gopls version && pyright --version

WORKDIR /src
CMD ["make", "integration-test"]
