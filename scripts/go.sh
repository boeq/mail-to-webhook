#!/bin/sh
# Runs the Go toolchain: a local `go` when there is one, otherwise the official image.
set -eu
if command -v go >/dev/null 2>&1; then
  exec go "$@"
fi
root=$(cd "$(dirname "$0")/.." && pwd)
cache="${M2W_GO_CACHE:-$HOME/.cache/m2w-go}"
mkdir -p "$cache/gopath" "$cache/build"
exec docker run --rm --user "$(id -u):$(id -g)" \
  -v "$root":/src -w /src \
  -v "$cache/gopath":/gopath -v "$cache/build":/gocache \
  -e GOPATH=/gopath -e GOCACHE=/gocache -e HOME=/tmp -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  golang:1.27 go "$@"
