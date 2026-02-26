#!/usr/bin/env bash
set -euo pipefail

ROLE="${1:-client}"
DURATION="${2:-21600}"
INTERVAL="${3:-20}"
OUT_BASE="${4:-/var/log/mpquic-diag}"
START_TS="$(date +%Y%m%d_%H%M%S)"
OUT_DIR="${OUT_BASE}-${ROLE}-${START_TS}"

if [[ "${ROLE}" != "client" && "${ROLE}" != "server" ]]; then
  echo "usage: $0 <client|server> [duration_sec] [interval_sec] [out_dir_prefix]" >&2
  exit 1
fi

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  exec sudo -E "$0" "$@"
fi

mkdir -p "$OUT_DIR"

echo "[mpquic-long-diagnostics] role=${ROLE} duration=${DURATION}s interval=${INTERVAL}s out=${OUT_DIR}"

# Static snapshot
{
  echo "=== start $(date -Is) ==="
  hostname
  uname -a
  ip -br a
  ip rule show
  ip route show
  for t in 100 101 102 103 104 105; do
    echo "--- table $t ---"
    ip route show table "$t" || true
  done
  ss -unap | grep mpquic || true
  nft list ruleset || true
} >"$OUT_DIR/startup.txt" 2>&1

# Continuous unit logs
JOURNAL_LOG="$OUT_DIR/journal-follow.log"
timeout "${DURATION}s" journalctl -f \
  -u mpquic@1.service -u mpquic@2.service -u mpquic@3.service \
  -u mpquic@4.service -u mpquic@5.service -u mpquic@6.service \
  --output=short-iso >"$JOURNAL_LOG" 2>&1 &
JOURNAL_PID=$!

SNAP_LOG="$OUT_DIR/snapshots.log"
END_TS=$(( $(date +%s) + DURATION ))

while [[ $(date +%s) -lt $END_TS ]]; do
  {
    echo
    echo "=== snapshot $(date -Is) ==="
    for i in 1 2 3 4 5 6; do
      printf "@%s=" "$i"
      systemctl is-active "mpquic@${i}.service" || true
    done
    ip -4 -br a | egrep '^enp7s[3-8]|^mpq[1-6]' || true
    ss -unap | grep mpquic || true

    if [[ "$ROLE" == "client" ]]; then
      ip rule show | egrep '100[1-6]|lookup' || true
      for t in 100 101 102 103 104 105; do
        echo "--- table $t ---"
        ip route show table "$t" || true
      done
      for i in 3 4 5; do
        echo "--- ping mpq${i} peer ---"
        ping -I "mpq${i}" -c 1 -W 1 "10.200.${i}.2" || true
      done
      timeout 4 tcpdump -ni enp7s6 udp port 45004 -c 8 2>/dev/null || true
      timeout 4 tcpdump -ni enp7s7 udp port 45005 -c 8 2>/dev/null || true
    else
      for i in 3 4 5; do
        echo "--- ping mpq${i} peer ---"
        ping -I "mpq${i}" -c 1 -W 1 "10.200.${i}.1" || true
      done
    fi
  } >>"$SNAP_LOG" 2>&1

  sleep "$INTERVAL"
done

wait "$JOURNAL_PID" || true

echo "[mpquic-long-diagnostics] completed out=${OUT_DIR}"
