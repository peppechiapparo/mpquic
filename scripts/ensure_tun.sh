#!/usr/bin/env bash
set -euo pipefail

TUN_NAME="${1:-}"
TUN_CIDR="${2:-}"
TUN_MTU="${3:-1300}"

if [[ -z "$TUN_NAME" || -z "$TUN_CIDR" ]]; then
  echo "usage: $0 <tun_name> <tun_cidr> [mtu]" >&2
  exit 1
fi

# Ensure TUN exists with IFF_MULTI_QUEUE.
# Always recreate to guarantee the flag is set (can't query it via sysfs).
# Safe because ExecStartPre runs after the old process has stopped.
if ip link show dev "$TUN_NAME" >/dev/null 2>&1; then
  ip link set dev "$TUN_NAME" down 2>/dev/null || true
  ip tuntap del dev "$TUN_NAME" mode tun 2>/dev/null || true
fi
ip tuntap add dev "$TUN_NAME" mode tun multi_queue

ip addr replace "$TUN_CIDR" dev "$TUN_NAME"
ip link set dev "$TUN_NAME" mtu "$TUN_MTU"
ip link set dev "$TUN_NAME" up
