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

# Build-time source provenance. This Makefile always runs with its working
# directory inside the PUBLIC plumb repository, so these git commands describe
# this repository even when the build is invoked from the private plumb-ops
# superproject (`make -C plumb build`), which mounts it as a submodule. That is
# precisely why they are stamped explicitly: Go's own debug.ReadBuildInfo()
# resolves vcs.revision/vcs.modified against the OUTER module there, naming the
# wrong commit and calling a clean tree dirty. Outside a git checkout (a tarball
# build) REVISION is empty, and an unstamped revision is reported as unknown
# rather than as clean — the dirty stamp is only read alongside a revision.
# No build timestamp is injected, deliberately: dev builds stay reproducible.
#
# REVISION_DIRTY emits NOTHING when `git status` itself fails, so the binary
# reports the tree state as unknown rather than as clean. The distinction is
# reachable: `git rev-parse` can succeed while `git status` fails (an unreadable
# .git/index, say), and the obvious `test -n "$(git status --porcelain)"` form
# cannot tell an empty stdout that means "clean" from an empty stdout that means
# "the command died" — it stamps a positive claim of cleanliness about a tree it
# never managed to inspect. The assignment's exit status is git's, so one
# invocation both gates and measures.
REVISION       := $(shell git rev-parse HEAD 2>/dev/null)
REVISION_DIRTY := $(shell out=$$(git status --porcelain 2>/dev/null) && { test -n "$$out" && echo true || echo false; })
BUILD_CHANNEL  := dev

LDFLAGS   := -X github.com/plumbkit/plumb/internal/cli.Version=$(VERSION) \
             -X github.com/plumbkit/plumb/internal/cli.Revision=$(REVISION) \
             -X github.com/plumbkit/plumb/internal/cli.RevisionDirty=$(REVISION_DIRTY) \
             -X github.com/plumbkit/plumb/internal/cli.BuildChannel=$(BUILD_CHANNEL)

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

.PHONY: build web-ui web-ui-audit test test-race integration-test fuzz build-integration lint lint-cross check-size check-brief cover cover-report vuln tidy-check verify run clean tidy install install-hooks hooks codesign ts-wasm swift-wasm install-clients clients-test clients-test-auth clients-test-conformance build-clients docker-integration docker-cleanroom site blog demo-gif

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

# fuzz runs every fuzz target in the tree for FUZZTIME each (default 60s).
#
# Targets are DISCOVERED, not listed. `go test` fuzzes one target per invocation
# (`-fuzz .` over ./... refuses with "matches more than one target"), so this has
# to iterate either way — and a hand-maintained list silently stops covering a
# target the day someone adds one, which is the failure mode a fuzzing target can
# least afford. Discovery also keeps this independent of which branch a given
# target happens to land on.
#
# Not in `verify`: fuzzing is time-boxed exploration, not a pass/fail gate, and
# the edit loop cannot afford it. The retained corpora under <pkg>/testdata/fuzz/
# ARE run by plain `make test` as ordinary cases, so every payload a fuzz run has
# already found stays a regression test whether or not anyone fuzzes again.
FUZZTIME ?= 60s

fuzz: $(TESTCACHE)
	@set -e; \
	found=0; \
	for f in $$(grep -rEl '^func Fuzz[A-Za-z0-9_]*\(' --include='*_test.go' . | sort); do \
		pkg=$$(dirname $$f); \
		for fn in $$(grep -hoE '^func Fuzz[A-Za-z0-9_]*' $$f | sed 's/^func //' | sort -u); do \
			found=1; \
			echo "==> fuzzing $$fn in $$pkg for $(FUZZTIME)"; \
			GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test $$pkg -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME); \
		done; \
	done; \
	if [ $$found -eq 0 ]; then echo "no fuzz targets found"; exit 1; fi

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

# clients-test-conformance is the deterministic, API-key-free real-client tier.
# It requires the pinned Codex and OpenCode binaries, drives both through a
# loopback scripted provider, and isolates the Go cache as well as all client and
# plumb state. Run it repeatedly before changing client capability evidence.
clients-test-conformance: $(TESTCACHE)
	@cache=$$(mktemp -d); trap 'rm -rf "$$cache"' EXIT; \
		GOCACHE="$$cache" GOTMPDIR=$(CURDIR)/$(TESTCACHE) \
		go test -tags=clients_conformance -timeout=10m -count=1 -v ./cmd/clientsmoke/...

# build-clients compiles and vets every clientsmoke build tag, which test/lint
# skip — keeping the on-demand harness from bitrotting (mirrors build-integration).
build-clients: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=clients ./cmd/clientsmoke/...
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=clients_e2e ./cmd/clientsmoke/...
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go vet -tags=clients_conformance ./cmd/clientsmoke/...

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

# lint-cross lints the OTHER supported OS's tree. golangci-lint only analyses
# files that match the current GOOS, so a Linux `make lint` never sees
# sandbox_darwin*.go or process_darwin*.go (and vice versa) — a real blind spot:
# a linter change validated on one platform can pass locally and fail CI's other
# matrix leg. Static analysis only; it never runs the other platform's tests.
#
# Run it after changing .golangci.yml or touching platform-constrained code.
# Not in `verify` — it roughly doubles lint time to cover nine files, and CI's
# two-OS matrix is the backstop. Windows is deliberately absent: plumb does not
# support it (internal/session's flock has no Windows implementation), so a
# GOOS=windows pass would report known, intentional breakage.
lint-cross:
	@other=$$(if [ "$$(go env GOOS)" = "darwin" ]; then echo linux; else echo darwin; fi); \
		echo "lint-cross: linting the $$other tree (current GOOS=$$(go env GOOS))"; \
		GOOS=$$other golangci-lint run && GOOS=$$other go vet ./... && \
		echo "lint-cross: OK ($$other tree clean)"

# check-size fails if any Go file exceeds its line rule — 600 for source, 900 for
# tests (with a grandfather baseline for files still awaiting a split). Keeps the
# standard from regressing — see scripts/check-file-size.sh.
check-size:
	./scripts/check-file-size.sh

# check-brief fails if AGENTS.md grows past its budget (200 lines / 32 KiB) —
# the brief is rules + pointers; reference detail lives in docs/. See
# scripts/check-agents-brief.sh.
check-brief:
	./scripts/check-agents-brief.sh

# cover measures statement coverage and fails below the floor in
# scripts/check-coverage.sh. Not in `verify` — it re-runs the whole suite with
# instrumentation, so it would roughly double the local edit loop; CI runs it on
# every push. Use `make cover-report` locally to see where the gaps are.
cover:
	./scripts/check-coverage.sh

cover-report:
	./scripts/check-coverage.sh --report

# vuln scans the module graph against the Go vulnerability database. Pinned to
# @latest deliberately: the value is knowing about a CVE published this morning,
# and the tool's own version is not part of the build.
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# web-ui-audit is vuln's npm counterpart for the embedded SPA. Two tiers, the
# same split CI's npm-audit job enforces: prod dependencies block at high
# (their code ships bundled in dist/), dev/build-time findings print but do
# not fail — advisories against build-time-only dependencies must not turn the
# tree red with no code change; the small prod tree still blocks, by the same
# posture as govulncheck. Reads the lockfile, so no npm ci first.
web-ui-audit:
	cd internal/web/ui && npm audit --omit=dev --audit-level=high
	-cd internal/web/ui && npm audit --audit-level=high

# tidy-check asserts go.mod/go.sum are already tidy. Note that `go mod tidy`
# mutates in place: on failure the tidied files are left in the working tree —
# deliberate, so the fix is to review and commit them.
tidy-check:
	@go mod tidy
	@git diff --exit-code -- go.mod go.sum \
		|| { echo "go.mod/go.sum not tidy — 'make tidy' changed them; commit the result"; exit 1; }
	@echo "tidy-check: OK (go.mod/go.sum unchanged by go mod tidy)"

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
# integration-tag compile pass (build-integration) + the file-size and brief
# guards + go.mod tidiness. Coverage (`make cover`) and vulnerabilities (`make
# vuln`) are deliberately NOT here — the first doubles the suite runtime, the
# second needs the network; CI runs both on every push.
verify: build test lint build-integration build-clients check-size check-brief tidy-check

# hooks is an alias for install-hooks — the ops-root Makefile uses `hooks-ops`
# for its own hook, and the asymmetry is a recurring stumble.
hooks: install-hooks

install-hooks:
	@hooks="$$(git rev-parse --git-path hooks)"; \
	mkdir -p "$$hooks"; \
	cp scripts/pre-commit "$$hooks/pre-commit"; \
	chmod +x "$$hooks/pre-commit"; \
	echo "Pre-commit hook installed at $$hooks/pre-commit. Run 'make verify' before every push."
