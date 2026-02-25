#!/usr/bin/env bash
set -euo pipefail

changed=0

restart_instance() {
  local inst="$1"
  systemctl restart "mpquic@${inst}.service" || true
  changed=1
}

ensure_tun_instance() {
  local tun_name="$1"
  local tun_cidr="$2"
  local tun_mtu="$3"

  /usr/local/lib/mpquic/ensure_tun.sh "$tun_name" "$tun_cidr" "$tun_mtu" >/dev/null 2>&1 || return 1
  return 0
}

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

  env_file="/etc/mpquic/instances/${i}.env"
  tun_name="$(grep -E '^TUN_NAME=' "$env_file" | cut -d= -f2- || true)"
  tun_cidr="$(grep -E '^TUN_CIDR=' "$env_file" | cut -d= -f2- || true)"
  tun_mtu="$(grep -E '^TUN_MTU=' "$env_file" | cut -d= -f2- || true)"
  tun_mtu="${tun_mtu:-1300}"

  if [[ -n "$tun_name" && -n "$tun_cidr" ]]; then
    if ! ensure_tun_instance "$tun_name" "$tun_cidr" "$tun_mtu"; then
      restart_instance "$i"
      continue
    fi
  fi

  if ! systemctl is-active --quiet "$svc"; then
    restart_instance "$i"
  elif ! tun_healthy "$i"; then
    restart_instance "$i"
  elif journalctl -u "$svc" --since '-90 seconds' --no-pager 2>/dev/null | grep -q 'write tun: input/output error'; then
    restart_instance "$i"
  fi
done

if [ "$changed" -eq 1 ] && systemctl list-unit-files | grep -q '^mpquic-vps-routes\.service'; then
  systemctl restart mpquic-vps-routes.service || true
fi

exit 0
