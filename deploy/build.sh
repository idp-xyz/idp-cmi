#!/usr/bin/env bash
# Build the idp-cmi image offline: a static binary on the host, then docker build --network none.
# Usage: bash deploy/build.sh [TAG]     (default idp-cmi:<commit or dev>)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
[[ -z "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]] || VERSION="$VERSION-dirty"
TAG="${1:-idp-cmi:$VERSION}"
OUT=build/image
umask 022   # the context's modes become the image's: world-readable files, traversable directories
rm -rf "$OUT" && mkdir -p "$OUT"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/idp-cmi" ./cmd/idp-cmi
cp /etc/ssl/certs/ca-certificates.crt "$OUT/" && chmod 644 "$OUT/ca-certificates.crt" && chmod 755 "$OUT"
cp deploy/Dockerfile "$OUT/"
docker build --network none -q -t "$TAG" "$OUT" > /dev/null
echo "OK: $TAG $(docker image inspect --format '{{.Id}}' "$TAG" | cut -c1-19)… idp-cmi sha256 $(sha256sum "$OUT/idp-cmi" | cut -c1-12)…"
