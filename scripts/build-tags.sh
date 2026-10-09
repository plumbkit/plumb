#!/usr/bin/env bash
# Print, comma-separated, every custom build tag the tree's //go:build lines
# use: the ones a plain `go vet` or `golangci-lint run` never enables
# (integration, clients, parity, race, ...). GOOS/GOARCH names, unix, cgo, the
# compiler names and go1.N release tags are dropped; those vary by platform and
# toolchain, not by flag. `make lint-tags` lints with this list, and works it out
# each time, so a tag added later is linted without anyone remembering to.
set -euo pipefail

platform="$(go tool dist list | tr '/' '\n' | sort -u)"
skip="$(printf '%s\n' $platform unix cgo gc gccgo ignore | sort -u)"

git grep -h -E '^//go:build ' -- '*.go' |
	sed 's#^//go:build ##' |
	tr -c 'A-Za-z0-9_.\n' '\n' |
	grep -E '^[A-Za-z_][A-Za-z0-9_]*$' |
	sort -u |
	comm -23 - <(printf '%s\n' "$skip") |
	paste -sd, -
