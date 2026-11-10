#!/usr/bin/env bash
set -euo pipefail

mkdir -p dist
for os in windows linux darwin; do
  for arch in amd64 arm64; do
    extension=""
    if [[ "$os" == "windows" ]]; then extension=".exe"; fi
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" \
      -o "dist/${os}-${arch}${extension}" ./cmd/gh-actions-archive-cache
  done
done
