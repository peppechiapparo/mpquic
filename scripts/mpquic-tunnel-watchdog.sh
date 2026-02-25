#!/usr/bin/env bash
set -euo pipefail

WAN_DEVS=("enp7s3" "enp7s4" "enp7s5" "enp7s6" "enp7s7" "enp7s8")
INSTANCES=("1" "2" "3" "4" "5" "6")

have_ipv4() {
  ip -4 addr show dev "$1" 2>/dev/null | grep -q "inet "
}

changed=0

for idx in "${!WAN_DEVS[@]}"; do
  dev="${WAN_DEVS[$idx]}"
  inst="${INSTANCES[$idx]}"
  svc="mpquic@${inst}.service"

  if have_ipv4 "$dev"; then
    if ! systemctl is-active --quiet "$svc"; then
      systemctl restart "$svc" || true
      changed=1
    fi
  else
    if systemctl is-active --quiet "$svc"; then
      systemctl stop "$svc" || true
      changed=1
    fi
  fi
done

if [ "$changed" -eq 1 ] && systemctl list-unit-files | grep -q '^mpquic-routing\.service'; then
  systemctl restart mpquic-routing.service || true
fi

exit 0
