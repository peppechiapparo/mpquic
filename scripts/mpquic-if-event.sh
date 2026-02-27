#!/usr/bin/env bash
set -euo pipefail

iface="${1:-}"
state="${2:-}"

if [ -z "$iface" ] || [ -z "$state" ]; then
  exit 0
fi

instance=""
case "$iface" in
  enp7s3) instance="1" ;;
  enp7s4) instance="2" ;;
  enp7s5) instance="3" ;;
  enp7s6) instance="4" ;;
  enp7s7) instance="5" ;;
  enp7s8) instance="6" ;;
  *) exit 0 ;;
esac

svc="mpquic@${instance}.service"

run_priv() {
  if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    "$@"
  else
    sudo -n "$@"
  fi
}

has_global_ipv4() {
  ip -4 -o addr show dev "$iface" scope global 2>/dev/null | grep -q 'inet '
}

wait_global_ipv4() {
  local timeout_s="${1:-15}"
  local waited=0
  while (( waited < timeout_s )); do
    if has_global_ipv4; then
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

recover_dhcp_ipv4() {
  if has_global_ipv4; then
    return 0
  fi

  if command -v networkctl >/dev/null 2>&1 && systemctl is-active --quiet systemd-networkd.service; then
    run_priv networkctl reconfigure "$iface" >/dev/null 2>&1 || true
    run_priv networkctl renew "$iface" >/dev/null 2>&1 || true
  fi

  if ! has_global_ipv4 && command -v ifup >/dev/null 2>&1; then
    run_priv ifup "$iface" >/dev/null 2>&1 || true
  fi

  if ! has_global_ipv4 && command -v dhclient >/dev/null 2>&1; then
    run_priv timeout 20 dhclient -4 -1 "$iface" >/dev/null 2>&1 || true
  fi

  wait_global_ipv4 15
}

if [[ "$state" == "up" || "$state" == "routable" || "$state" == "configured" || "$state" == "dhcp4-change" ]]; then
  if recover_dhcp_ipv4; then
    run_priv systemctl restart "$svc" || true
  else
    run_priv systemctl stop "$svc" || true
  fi
elif [[ "$state" == "down" || "$state" == "degraded" || "$state" == "off" || "$state" == "no-carrier" ]]; then
  run_priv systemctl stop "$svc" || true
fi

if systemctl list-unit-files | grep -q '^mpquic-routing\.service'; then
  run_priv systemctl restart mpquic-routing.service || true
fi

exit 0
