#!/usr/bin/env bash
set -euo pipefail

TUN_NAME="${1:-}"
TUN_CIDR="${2:-}"
TUN_MTU="${3:-1300}"

if [[ -z "$TUN_NAME" || -z "$TUN_CIDR" ]]; then
  echo "usage: $0 <tun_name> <tun_cidr> [mtu]" >&2
  exit 1
fi

if ! ip link show dev "$TUN_NAME" >/dev/null 2>&1; then
  ip tuntap add dev "$TUN_NAME" mode tun
fi

ip addr replace "$TUN_CIDR" dev "$TUN_NAME"
ip link set dev "$TUN_NAME" mtu "$TUN_MTU"
ip link set dev "$TUN_NAME" up
