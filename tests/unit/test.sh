#!/bin/bash
set -euxo pipefail

# tmt runs tests from a copy of the repository; TMT_TREE is its root.
cd "${TMT_TREE:-$(dirname "$0")/../..}"

go version
go vet ./...
test -z "$(gofmt -l .)"
go test -count=1 ./...
# -race needs cgo, hence gcc.
CGO_ENABLED=1 go test -count=1 -race ./...
