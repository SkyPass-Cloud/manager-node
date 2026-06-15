#!/usr/bin/env bash
# Cross-compile the skypassd binary for Linux from any host.
# Output goes to ./dist. Run on Windows via Git Bash, or on Linux/mac.
set -euo pipefail

VERSION="${1:-dev}"
OUT="dist"
mkdir -p "$OUT"

LDFLAGS="-s -w -X main.version=${VERSION}"

build() {
  local arch="$1"
  echo "==> building linux/${arch}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" \
    -o "${OUT}/skypassd-linux-${arch}" \
    ./cmd/skypassd
}

build amd64
build arm64

echo "==> artifacts:"
ls -lh "$OUT"
