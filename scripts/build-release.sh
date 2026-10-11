#!/bin/sh
# Cross-compile static release binaries into dist/.
set -eu
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do
  os="${target%/*}"; arch="${target#*/}"
  ext=""; [ "$os" = windows ] && ext=".exe"
  out="dist/lite-model-router-${VERSION}-${os}-${arch}${ext}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$out" .
  echo "built $out"
done
