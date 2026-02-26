#!/usr/bin/env bash
set -euo pipefail

ROLE="${1:-}"
MODE="${2:-fix}"

if [[ "$ROLE" != "client" && "$ROLE" != "server" ]]; then
  echo "usage: $0 <client|server> [check|fix]" >&2
  exit 1
fi

if [[ "$MODE" != "check" && "$MODE" != "fix" ]]; then
  echo "usage: $0 <client|server> [check|fix]" >&2
  exit 1
fi

INSTANCES=("1" "2" "3" "4" "5" "6")
WAN_DEVS=("enp7s3" "enp7s4" "enp7s5" "enp7s6" "enp7s7" "enp7s8")

changed=0
healthy=0
degraded=0

have_ipv4() {
  ip -4 addr show dev "$1" 2>/dev/null | grep -q "inet "
}

have_carrier() {
  local dev="$1"
  local carrier_file="/sys/class/net/${dev}/carrier"
  if [[ -r "$carrier_file" ]]; then
    [[ "$(cat "$carrier_file" 2>/dev/null || echo 0)" = "1" ]]
    return
  fi
  ip link show dev "$dev" 2>/dev/null | grep -q "LOWER_UP"
}

wan_usable() {
  local dev="$1"
  have_ipv4 "$dev" && have_carrier "$dev"
}

tun_healthy() {
  local inst="$1"
  local env_file="/etc/mpquic/instances/${inst}.env"

  [[ -r "$env_file" ]] || return 1

  local tun_name
  local tun_cidr
  tun_name="$(awk -F= '/^TUN_NAME=/{print $2}' "$env_file" | tail -n 1)"
  tun_cidr="$(awk -F= '/^TUN_CIDR=/{print $2}' "$env_file" | tail -n 1)"

  [[ -n "$tun_name" && -n "$tun_cidr" ]] || return 1

  ip link show dev "$tun_name" >/dev/null 2>&1 || return 1
  ip link show dev "$tun_name" | head -n1 | grep -q "UP" || return 1
  ip -4 addr show dev "$tun_name" | grep -q "inet ${tun_cidr}" || return 1

  return 0
}

svc_active() {
  systemctl is-active --quiet "mpquic@$1.service"
}

restart_instance() {
  local inst="$1"
  systemctl restart "mpquic@$inst.service" || true
  changed=1
}

stop_instance() {
  local inst="$1"
  systemctl stop "mpquic@$inst.service" || true
  changed=1
}

ensure_server_firewall() {
  if ! command -v nft >/dev/null 2>&1; then
    return
  fi

  if ! nft list chain inet filter input >/dev/null 2>&1; then
    return
  fi

  if ! nft list chain inet filter input | grep -q 'udp dport 45001-45006 accept'; then
    if [[ "$MODE" == "fix" ]]; then
      nft add rule inet filter input udp dport 45001-45006 accept || true
      changed=1
    fi
  fi
}

echo "[mpquic-healthcheck] role=$ROLE mode=$MODE"

if [[ "$ROLE" == "server" ]]; then
  ensure_server_firewall
fi

for idx in "${!INSTANCES[@]}"; do
  inst="${INSTANCES[$idx]}"
  wan_dev="${WAN_DEVS[$idx]}"

  if [[ "$ROLE" == "client" ]]; then
    if wan_usable "$wan_dev"; then
      if ! svc_active "$inst" || ! tun_healthy "$inst"; then
        [[ "$MODE" == "fix" ]] && restart_instance "$inst"
      fi
    else
      if svc_active "$inst"; then
        [[ "$MODE" == "fix" ]] && stop_instance "$inst"
      fi
    fi
  else
    if ! svc_active "$inst" || ! tun_healthy "$inst"; then
      [[ "$MODE" == "fix" ]] && restart_instance "$inst"
    fi
  fi
done

if [[ "$changed" -eq 1 ]]; then
  if [[ "$ROLE" == "client" ]]; then
    systemctl list-unit-files | grep -q '^mpquic-routing\.service' && systemctl restart mpquic-routing.service || true
  else
    systemctl list-unit-files | grep -q '^mpquic-vps-routes\.service' && systemctl restart mpquic-vps-routes.service || true
  fi
fi

for idx in "${!INSTANCES[@]}"; do
  inst="${INSTANCES[$idx]}"
  wan_dev="${WAN_DEVS[$idx]}"

  if [[ "$ROLE" == "client" ]]; then
    if wan_usable "$wan_dev"; then
      if svc_active "$inst" && tun_healthy "$inst"; then
        healthy=$((healthy + 1))
      else
        degraded=$((degraded + 1))
      fi
    else
      if ! svc_active "$inst"; then
        healthy=$((healthy + 1))
      else
        degraded=$((degraded + 1))
      fi
    fi
  else
    if svc_active "$inst" && tun_healthy "$inst"; then
      healthy=$((healthy + 1))
    else
      degraded=$((degraded + 1))
    fi
  fi
done

echo "[mpquic-healthcheck] healthy=$healthy degraded=$degraded changed=$changed"

if [[ "$degraded" -gt 0 ]]; then
  exit 2
fi

exit 0
