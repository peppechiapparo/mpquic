#!/usr/bin/env bash
set -euo pipefail

OUT_DIR="${1:-/etc/mpquic/tls}"
CN="${2:-mpquic-server}"
DAYS="${3:-825}"

mkdir -p "$OUT_DIR"

openssl req -x509 -newkey rsa:2048 -sha256 -nodes \
  -keyout "$OUT_DIR/server.key" \
  -out "$OUT_DIR/server.crt" \
  -days "$DAYS" \
  -subj "/CN=${CN}" \
  -addext "subjectAltName=DNS:${CN}"

cp "$OUT_DIR/server.crt" "$OUT_DIR/ca.crt"
chmod 600 "$OUT_DIR/server.key"
chmod 644 "$OUT_DIR/server.crt" "$OUT_DIR/ca.crt"

echo "TLS material generated in $OUT_DIR"
