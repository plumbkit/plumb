# verify.Dockerfile — the toolchain for `make verify-full-container`.
#
# `make verify-full` runs suites that start plumb daemons of their own
# (cmd/smoke, cmd/clientsmoke), and a developer machine must run exactly one
# plumb daemon, so the full gate runs here instead (or in CI). The image holds
# only tools, never the source: scripts/verify-full-container.sh streams the
# working tree in at run time.
#
# Every tool is pinned, so a green run means the same thing tomorrow. Keep Go in
# step with go.mod's toolchain line, golangci-lint with GOLANGCI_LINT_VERSION in
# .github/workflows/ci.yml, and the language servers with the integration job.
FROM golang:1.27.2-bookworm

# Node only to install the npm-published language servers.
COPY --from=node:22.23.3-bookworm /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22.23.3-bookworm /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

ARG GOPLS_VERSION=v0.23.0
ARG GOLANGCI_LINT_VERSION=v2.14.0
RUN go install "golang.org/x/tools/gopls@${GOPLS_VERSION}" \
 && go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}" \
 && go clean -cache -modcache

# typescript@5: plain `typescript` resolves to 7, which ships no tsserver (see
# the integration job in ci.yml).
RUN npm install -g --no-audit --no-fund \
      pyright@1.1.414 typescript-language-server@6.0.2 typescript@5.9.3 vscode-langservers-extracted@4.10.0

ENV PATH="/go/bin:${PATH}"

# Fail the image, not the run, when a tool is broken.
RUN gopls version && golangci-lint --version && pyright --version \
 && typescript-language-server --version && command -v vscode-html-language-server
