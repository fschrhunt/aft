#!/bin/sh
# Shared local/CI task entry point. Only fmt edits source; no task publishes.
set -eu
cd "$(dirname "$0")"

usage() {
  echo 'usage: ./x [fmt [--check]|check|audit|test|setup [FLAGS]|try [TEST]|bench|docs|servers [VERSION]|build|release-check [vX.Y.Z]|dist vX.Y.Z]' >&2
  exit 2
}

task=${1:-check}
if [ "$#" -gt 0 ]; then shift; fi
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
if [ "$task" = servers ] && [ "$#" -eq 1 ]; then version=$1; fi
# The commit time orders builds, so a client never replaces a newer server
# with its own older one. An uncommitted build is dated by its commit too:
# dated by the clock it would outrank every release until that date passed.
built=$(git log -1 --format=%ct 2>/dev/null || date +%s)
ldflags="-s -w -X main.version=$version -X github.com/fschrhunt/aft/internal/session.Version=$version -X github.com/fschrhunt/aft/internal/session.Built=$built"

case "$task" in
  fmt)
    [ "$#" -le 1 ] || usage
    if [ "$#" -eq 0 ]; then
      gofmt -w cmd internal
    elif [ "$1" = --check ]; then
      files=$(gofmt -l cmd internal)
      if [ -n "$files" ]; then printf 'Run ./x fmt:\n%s\n' "$files" >&2; exit 1; fi
    else usage; fi
    ;;
  check)
    [ "$#" -eq 0 ] || usage
    ./x fmt --check
    go vet ./...
    ./x test
    ./x docs
    ./x build
    ;;
  audit)
    [ "$#" -eq 0 ] || usage
    exec go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
    ;;
  test)
    [ "$#" -eq 0 ] || usage
    # The install package test embeds the cross-compiled servers; build them if
    # they are not present yet (a fresh checkout or CI).
    [ -e internal/session/install/embed/aft-server-linux-amd64.gz ] || ./x servers
    go test -race ./...
    python3 -m unittest discover -s scripts -p 'test_*.py'
    python3 -m unittest discover -s .github/release -p 'test_*.py'
    ;;
  setup)
    # Preview setup with private configuration and no remote access.
    ./x build >/dev/null
    exec ./bin/aft selftest setup "$@"
    ;;
  try)
    # Run a local check or open the interactive test menu.
    ./x build >/dev/null
    exec ./bin/aft selftest "$@"
    ;;
  bench)
    # Run the benchmarks (microbenchmarks and the end-to-end typing-latency
    # benchmark) and print the results.
    [ "$#" -eq 0 ] || usage
    exec go test -run '^$' -bench . -benchmem ./...
    ;;
  docs)
    [ "$#" -eq 0 ] || usage
    exec python3 scripts/check_docs.py
    ;;
  servers)
    # Cross-compile the server for every supported host platform and gzip each,
    # so the aft binary embeds them compressed (about half the size) and
    # decompresses on install. Packaging supplies the matching release version.
    [ "$#" -le 1 ] || usage
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
      os=${target%/*}; arch=${target#*/}
      out="internal/session/install/embed/aft-server-$os-$arch"
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/aft-server
      gzip -9 -n -f "$out"
    done
    ;;
  build)
    [ "$#" -eq 0 ] || usage
    ./x servers
    mkdir -p bin
    go build -trimpath -ldflags "$ldflags" -o bin/aft ./cmd/aft
    ;;
  release-check)
    [ "$#" -le 1 ] || usage
    exec python3 .github/release/release.py check "$@"
    ;;
  dist)
    [ "$#" -eq 1 ] || usage
    exec python3 .github/release/release.py dist "$1"
    ;;
  *) usage ;;
esac
