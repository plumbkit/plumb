BINARY := plumb
CMD    := ./cmd/plumb

.PHONY: build test test-race lint run clean tidy install-hooks

build:
	go build -o $(BINARY) $(CMD)

test:
	go test ./...

test-race:
	go test -race ./...

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
