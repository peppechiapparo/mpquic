#!/usr/bin/env bash
set -euo pipefail

changed=0

tun_healthy() {
  local inst="$1"
  local env_file="/etc/mpquic/instances/${inst}.env"

  if [[ ! -f "$env_file" ]]; then
    return 1
  fi

  local tun_name
  local tun_cidr
  tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2-)"
  tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2-)"

  if [[ -z "$tun_name" || -z "$tun_cidr" ]]; then
    return 1
  fi

  ip link show dev "$tun_name" >/dev/null 2>&1 || return 1
  ip link show dev "$tun_name" | head -n1 | grep -q "UP" || return 1
  ip -4 addr show dev "$tun_name" | grep -q "inet ${tun_cidr}" || return 1

  return 0
}

for i in 1 2 3 4 5 6; do
  svc="mpquic@${i}.service"
  if ! systemctl is-active --quiet "$svc"; then
    systemctl restart "$svc" || true
    changed=1
  elif ! tun_healthy "$i"; then
    systemctl restart "$svc" || true
    changed=1
  fi
done

if [ "$changed" -eq 1 ] && systemctl list-unit-files | grep -q '^mpquic-vps-routes\.service'; then
  systemctl restart mpquic-vps-routes.service || true
fi

exit 0
