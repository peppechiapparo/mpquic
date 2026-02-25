#!/usr/bin/env bash
set -euo pipefail

INSTANCE_ID="${1:-}"
if [[ -z "$INSTANCE_ID" ]]; then
  echo "usage: $0 <instance_id>" >&2
  exit 1
fi

SRC_TEMPLATE="/etc/mpquic/instances/${INSTANCE_ID}.yaml.tpl"
SRC_LEGACY="/etc/mpquic/instances/${INSTANCE_ID}.yaml"
OUT_DIR="/run/mpquic"
OUT_FILE="${OUT_DIR}/${INSTANCE_ID}.yaml"

if [[ -f "$SRC_TEMPLATE" ]]; then
  SRC_FILE="$SRC_TEMPLATE"
elif [[ -f "$SRC_LEGACY" ]]; then
  SRC_FILE="$SRC_LEGACY"
else
  echo "config source not found for instance ${INSTANCE_ID} (expected ${SRC_TEMPLATE} or ${SRC_LEGACY})" >&2
  exit 1
fi

if grep -q 'VPS_PUBLIC_IP' "$SRC_FILE" && [[ -z "${VPS_PUBLIC_IP:-}" ]]; then
  echo "VPS_PUBLIC_IP is required but not set (set it in /etc/mpquic/global.env)" >&2
  exit 1
fi

install -d "$OUT_DIR"
sed "s|VPS_PUBLIC_IP|${VPS_PUBLIC_IP:-}|g" "$SRC_FILE" > "$OUT_FILE"
