BINARY    := plumb
CMD       := ./cmd/plumb
TESTCACHE := .testcache
# Try an exact git tag first (release builds), then fall back to VERSION file,
# then fall back to the short commit hash.
VERSION   := $(shell git describe --tags --exact-match 2>/dev/null || cat VERSION 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS   := -X github.com/golimpio/plumb/internal/cli.Version=$(VERSION)

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
CODESIGN_BUNDLE  := com.golimpio.plumb

.PHONY: build test test-race integration-test build-integration lint verify run clean tidy install-hooks codesign

$(TESTCACHE):
	mkdir -p $(TESTCACHE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)
ifeq ($(UNAME_S),Darwin)
	@$(MAKE) --no-print-directory codesign
endif

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

lint:
	golangci-lint run

run:
	go run $(CMD)

clean:
	rm -f $(BINARY)

tidy:
	go mod tidy

# verify is the definition of "ready to commit": build + test + lint + an
# integration-tag compile pass (build-integration) in one target.
verify: build test lint build-integration

install-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed. Run 'make verify' before every push."
