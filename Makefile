BINARY    := plumb
CMD       := ./cmd/plumb
TESTCACHE := .testcache

# install destination. PREFIX defaults to ~/.local (needs no sudo, is not
# Homebrew-managed, and aligns with the XDG dirs plumb already uses); override
# for a system location, e.g. `make install PREFIX=/usr/local` (needs sudo).
# DESTDIR is honoured for staged/packaged installs.
PREFIX    ?= $(HOME)/.local
BINDIR    := $(DESTDIR)$(PREFIX)/bin
# Try an exact git tag first (release builds), then fall back to VERSION file,
# then fall back to the short commit hash.
VERSION   := $(shell git describe --tags --exact-match 2>/dev/null || cat VERSION 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS   := -X github.com/plumbkit/plumb/internal/cli.Version=$(VERSION)

# macOS-only codesign settings. CODESIGN_IDENTITY can be:
#   - unset/empty: ad-hoc sign (`-s -`). Gives the binary a stable identifier
#     but the cdhash changes on every rebuild, so macOS may still re-prompt
#     for TCC consent (Documents, Pictures, …) after each rebuild.
#   - the name of a self-signed cert in your login keychain (recommended for
#     local dev): TCC keys grants to the cert's Designated Requirement, so
#     grants survive rebuilds. Create one via Keychain Access:
#       Keychain Access → Certificate Assistant → Create a Certificate
#       Name: plumb-dev   Identity Type: Self Signed Root
#       Certificate Type: Code Signing
#     Then build with: CODESIGN_IDENTITY=plumb-dev make build
#   - a real Apple Developer ID identity (for distribution).
UNAME_S          := $(shell uname -s)
CODESIGN_ID      := $(if $(CODESIGN_IDENTITY),$(CODESIGN_IDENTITY),-)
CODESIGN_BUNDLE  := com.plumbkit.plumb

.PHONY: build web-ui test test-race integration-test build-integration lint check-size verify run clean tidy install install-hooks codesign ts-wasm swift-wasm install-clients clients-test clients-test-auth build-clients docker-integration docker-cleanroom site blog demo-gif

$(TESTCACHE):
	mkdir -p $(TESTCACHE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)
ifeq ($(UNAME_S),Darwin)
	@$(MAKE) --no-print-directory codesign
endif

# web-ui builds the embedded Svelte SPA into internal/web/ui/dist, which the
# Go binary //go:embed's via internal/web/assets.go. A committed placeholder
# index.html keeps a bare `go build` compiling, so this is only needed to pick
# up frontend changes; run it before `make build` after editing the SPA.
web-ui:
	cd internal/web/ui && npm ci && npm run build

# codesign signs the built binary on macOS. Stable identifier (CODESIGN_BUNDLE)
# means TCC associates consent with "this thing called plumb" instead of with
# a raw file path; with a named identity it also survives rebuilds. On
# non-Darwin this is a no-op so the recipe is safe to call unconditionally.
codesign:
ifeq ($(UNAME_S),Darwin)
	codesign --force --sign "$(CODESIGN_ID)" \
		--identifier "$(CODESIGN_BUNDLE)" \
		--preserve-metadata=entitlements,requirements,flags,runtime \
		$(BINARY)
	@codesign -dv $(BINARY) 2>&1 | sed 's/^/  /' || true
else
	@echo "codesign: skipping on $(UNAME_S) (macOS-only)"
endif

test: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test ./...

test-race: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test -race ./...

integration-test: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test -tags=integration -timeout=10m ./...

# build-integration compiles and vets the //go:build integration files, which
# test/lint skip without the tag — catching an integration-only compile error or
# an uncommitted integration helper locally, before CI's integration job. (The
# gap that let 0.8.1 commit a cmd/smoke that did not build under the tag.)
build-integration: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=integration ./...

# install-clients installs the MCP client CLIs the clientsmoke harness drives
# (idempotent; never configures API keys). See scripts/install-clients.sh.
install-clients:
	./scripts/install-clients.sh

# clients-test is the on-demand CONNECTION tier: it confirms each installed
# client CLI completes the MCP handshake with plumb, non-interactively and
# without API keys. Uninstalled clients (and those lacking an auth-free probe)
# are skipped. See cmd/clientsmoke.
clients-test: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test -tags=clients -timeout=15m -v ./cmd/clientsmoke/...

# clients-test-auth is the LLM AUTH tier: it drives each client headless to force
# a real plumb tool call. Runs only the clients whose API key is exported (e.g.
# OPENAI_API_KEY for most; ANTHROPIC_API_KEY/GEMINI_API_KEY/CURSOR_API_KEY for
# claude/gemini/cursor); the rest skip. Costs money.
clients-test-auth: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test -tags=clients_e2e -timeout=20m -v ./cmd/clientsmoke/...

# build-clients compiles and vets both clientsmoke build tags, which test/lint
# skip — keeping the on-demand harness from bitrotting (mirrors build-integration).
build-clients: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=clients ./cmd/clientsmoke/...
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=clients_e2e ./cmd/clientsmoke/...

# ── Docker-based Linux testing (opt-in; never part of `make verify`). ─────────
# plumb is developed on macOS; these run the Linux suites in a container so a
# macOS developer can reproduce them locally. arm64-native by default on Apple
# Silicon; set DOCKER_PLATFORM=linux/amd64 for amd64 fidelity (QEMU-emulated).
DOCKER_PLATFORM      ?=
DOCKER_PLATFORM_FLAG := $(if $(DOCKER_PLATFORM),--platform $(DOCKER_PLATFORM),)

# docker-integration mirrors the CI `integration` job (real gopls + pyright) on
# Linux, locally. The repo is bind-mounted so the image always reflects the
# working tree; named volumes cache the Go build + module caches across reruns.
docker-integration:
	docker build $(DOCKER_PLATFORM_FLAG) -f build/docker/integration.Dockerfile -t plumb-integration build/docker
	docker run --rm $(DOCKER_PLATFORM_FLAG) \
		-v "$(CURDIR)":/src \
		-v plumb-gocache:/root/.cache/go-build \
		-v plumb-gomod:/go/pkg/mod \
		plumb-integration

# docker-cleanroom proves a fresh Debian with NO toolchain can install and run
# plumb end-to-end: a multi-stage build compiles the binary, then a slim runtime
# (bash + python3 only) drives the two-agents MCP demo. The demo's exit code is
# the verdict — the automatable form of "clean-VM verification" before tagging.
docker-cleanroom:
	docker build $(DOCKER_PLATFORM_FLAG) -f build/docker/cleanroom.Dockerfile -t plumb-cleanroom --build-arg VERSION=$(VERSION) .
	docker run --rm $(DOCKER_PLATFORM_FLAG) plumb-cleanroom

lint:
	golangci-lint run

# check-size fails if any non-test Go file exceeds the ~600-line rule (with a
# grandfather baseline for files still awaiting a split). Keeps the standard
# from regressing — see scripts/check-file-size.sh.
check-size:
	./scripts/check-file-size.sh

run:
	go run $(CMD)

clean:
	rm -f $(BINARY)

tidy:
	go mod tidy

# install builds plumb and copies the freshly-codesigned binary onto PATH, so the
# daemon no longer runs out of the build tree (where a rebuild or a stray git
# checkout could swap the live binary). It copies the signed artifact rather than
# using `go install`, so macOS TCC consent stays stable across rebuilds. After
# installing, restart the daemon (`plumb restart --force`) and re-run
# `plumb setup <client>` so client configs point at the installed path.
install: build
	@install -d "$(BINDIR)"
	@install -m 0755 $(BINARY) "$(BINDIR)/$(BINARY)"
	@echo "installed $$("$(BINDIR)/$(BINARY)" version 2>/dev/null | tail -1) -> $(BINDIR)/$(BINARY)"
	@case ":$$PATH:" in \
		*":$(PREFIX)/bin:"*) ;; \
		*) printf 'note: %s is not on your PATH — add it (e.g. fish_add_path %s, or export PATH=\"%s:$$PATH\")\n' "$(PREFIX)/bin" "$(PREFIX)/bin" "$(PREFIX)/bin" ;; \
	esac

# ts-wasm regenerates the embedded TypeScript/TSX tree-sitter wasm from the
# vendored C sources. Dev-only — requires `zig`; building/running plumb needs
# only Go + wazero. Run after updating the vendored grammar or runtime.
ts-wasm:
	bash internal/topology/extractors/wasmts/csrc/build.sh

# swift-wasm regenerates the embedded Swift tree-sitter wasm (canonical
# alex-pinkus grammar + its C external scanner) from the vendored C sources.
# Dev-only — requires `zig`; building/running plumb needs only Go + wazero.
swift-wasm:
	bash internal/topology/extractors/wasmts/csrc/build-swift.sh

# demo-gif records docs/demos/daemon-respawn.sh headlessly and renders it to
# docs/assets/daemon-respawn.gif (the README's crash-resilience demo asset).
# Dev-only — requires `asciinema` and `agg` (brew install agg / AUR
# asciinema-agg-bin); the demo itself needs only bash + python3 + the built
# binary. Deterministic size via --window-size; --speed/--last-frame-duration
# tune readability. Idempotent — rerun after changing the demo script.
demo-gif: build
	@command -v asciinema >/dev/null || { echo "demo-gif: asciinema not installed"; exit 1; }
	@command -v agg >/dev/null || { echo "demo-gif: agg not installed"; exit 1; }
	@mkdir -p docs/assets
	rm -f .testcache/demo.cast
	PLUMB_BIN=$(CURDIR)/$(BINARY) asciinema rec --window-size 96x28 \
		--command='./docs/demos/daemon-respawn.sh' .testcache/demo.cast
	agg --speed 0.35 --last-frame-duration 6 --font-family "JetBrains Mono" \
		.testcache/demo.cast docs/assets/daemon-respawn.gif
	@rm -f .testcache/demo.cast
	@ls -la docs/assets/daemon-respawn.gif

# site (re)generates the landing-page TUI demo videos (light + dark, webm + mp4)
# from the asciicast at site/plumb_tui.cast into site/. Re-record with `asciinema
# rec site/plumb_tui.cast` (use ~100x26; see docs in the script), then run `make site`.
# Dev-only — requires `agg` (brew install agg), `ffmpeg`, and the Nerd font.
site: blog
	python3 scripts/build-tui-video.py

# blog renders the Markdown posts under site/blog/posts/ into styled HTML + the
# blog index (see scripts/build-blog.py). This is the same step CI runs before the
# Pages deploy. Needs Python 3.11+ and the deps in scripts/requirements.txt
# (pip install -r scripts/requirements.txt). Light — no agg/ffmpeg, unlike `site`.
blog:
	python3 scripts/build-blog.py

# verify is the definition of "ready to commit": build + test + lint + an
# integration-tag compile pass (build-integration) + the file-size guard.
verify: build test lint build-integration build-clients check-size

install-hooks:
	@hooks="$$(git rev-parse --git-path hooks)"; \
	mkdir -p "$$hooks"; \
	cp scripts/pre-commit "$$hooks/pre-commit"; \
	chmod +x "$$hooks/pre-commit"; \
	echo "Pre-commit hook installed at $$hooks/pre-commit. Run 'make verify' before every push."
