BINARY    := plumb
CMD       := ./cmd/plumb
TESTCACHE := .testcache
# Try an exact git tag first (release builds), then fall back to VERSION file,
# then fall back to the short commit hash.
VERSION   := $(shell git describe --tags --exact-match 2>/dev/null || cat VERSION 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS   := -X github.com/golimpio/plumb/internal/cli.Version=$(VERSION)

.PHONY: build test test-race lint run clean tidy install-hooks

$(TESTCACHE):
	mkdir -p $(TESTCACHE)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test ./...

test-race: $(TESTCACHE)
	GOTMPDIR=$(CURDIR)/$(TESTCACHE) go test -race ./...

lint:
	golangci-lint run

run:
	go run $(CMD)

clean:
	rm -f $(BINARY)

tidy:
	go mod tidy

install-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
