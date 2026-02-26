#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CFG_TEMPLATE="${1:-$ROOT_DIR/deploy/config/client/multipath.yaml}"
DURATION="${2:-25}"
TMP_CFG="/run/mpquic/multipath-smoke.yaml"
LOG_FILE="/tmp/mpquic-multipath-smoke.log"

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  exec sudo -E "$0" "$@"
fi

if [[ ! -f "$CFG_TEMPLATE" ]]; then
  echo "template non trovato: $CFG_TEMPLATE" >&2
  exit 1
fi

if grep -q 'VPS_PUBLIC_IP' "$CFG_TEMPLATE" && [[ -z "${VPS_PUBLIC_IP:-}" ]]; then
  echo "imposta VPS_PUBLIC_IP nell'ambiente (es. export VPS_PUBLIC_IP=1.2.3.4)" >&2
  exit 1
fi

install -d /run/mpquic
sed "s|VPS_PUBLIC_IP|${VPS_PUBLIC_IP:-}|g" "$CFG_TEMPLATE" > "$TMP_CFG"

MPQUIC_BIN="$ROOT_DIR/bin/mpquic"
if [[ ! -x "$MPQUIC_BIN" ]]; then
  MPQUIC_BIN="/usr/local/bin/mpquic"
fi

if [[ ! -x "$MPQUIC_BIN" ]]; then
  echo "binario mpquic non trovato (atteso $ROOT_DIR/bin/mpquic o /usr/local/bin/mpquic)" >&2
  exit 1
fi

set +e
timeout "${DURATION}s" "$MPQUIC_BIN" --config "$TMP_CFG" >"$LOG_FILE" 2>&1
RC=$?
set -e

if [[ "$RC" -ne 0 && "$RC" -ne 124 ]]; then
  echo "esecuzione fallita rc=$RC" >&2
  tail -n 80 "$LOG_FILE" >&2 || true
  exit "$RC"
fi

if ! grep -q 'connected multipath paths=' "$LOG_FILE"; then
  echo "smoke test FAIL: connessione multipath non stabilita" >&2
  tail -n 80 "$LOG_FILE" >&2 || true
  exit 2
fi

UP_COUNT=$(grep -c 'path up name=' "$LOG_FILE" || true)
if [[ "$UP_COUNT" -lt 1 ]]; then
  echo "smoke test FAIL: nessun path multipath up" >&2
  tail -n 80 "$LOG_FILE" >&2 || true
  exit 3
fi

echo "smoke test PASS: multipath connesso (paths up: $UP_COUNT)"
echo "log: $LOG_FILE"
