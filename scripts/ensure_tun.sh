#!/usr/bin/env bash
set -euo pipefail

TUN_NAME="${1:-}"
TUN_CIDR="${2:-}"
TUN_MTU="${3:-1300}"

if [[ -z "$TUN_NAME" || -z "$TUN_CIDR" ]]; then
  echo "usage: $0 <tun_name> <tun_cidr> [mtu]" >&2
  exit 1
fi

# Try to (re)create TUN with IFF_MULTI_QUEUE for parallel fd support.
# If the old device cannot be deleted (fd still held by a dying process),
# just reuse whatever exists — the Go code handles the fallback.
if ip link show dev "$TUN_NAME" >/dev/null 2>&1; then
  ip link set dev "$TUN_NAME" down 2>/dev/null || true
  ip tuntap del dev "$TUN_NAME" mode tun 2>/dev/null || true
fi

if ! ip link show dev "$TUN_NAME" >/dev/null 2>&1; then
  # Device gone (or never existed) — create fresh, prefer multi_queue
  if ! ip tuntap add dev "$TUN_NAME" mode tun multi_queue 2>/dev/null; then
    ip tuntap add dev "$TUN_NAME" mode tun
  fi
fi
# else: device still exists (could not be deleted), just reconfigure below

ip addr replace "$TUN_CIDR" dev "$TUN_NAME"
ip link set dev "$TUN_NAME" mtu "$TUN_MTU"
ip link set dev "$TUN_NAME" up
