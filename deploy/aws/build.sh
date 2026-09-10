#!/usr/bin/env bash
set -euo pipefail

# Cross-compiles both Lambda entrypoints for linux/arm64 (Graviton,
# provided.al2023 custom runtime) and zips each as bootstrap.zip. Safe to
# re-run; always rebuilds from source.

cd "$(dirname "$0")/../.."   # repo root
OUT_DIR="deploy/aws/build"
mkdir -p "$OUT_DIR"

build_one() {
  local name="$1" pkg="$2"
  echo "==> Building $name from $pkg"
  rm -f "$OUT_DIR/bootstrap"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/bootstrap" "$pkg"
  ( cd "$OUT_DIR" && rm -f "$name.zip" && zip -q -X "$name.zip" bootstrap && rm bootstrap )
  echo "==> Wrote $OUT_DIR/$name.zip"
}

build_one "makwatches-api" "./cmd/lambda"
build_one "makwatches-shipment-worker" "./cmd/shipment-worker"

echo "Build complete:"
ls -la "$OUT_DIR"/*.zip
