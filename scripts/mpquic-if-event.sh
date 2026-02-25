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

if [[ "$state" == "up" || "$state" == "routable" || "$state" == "dhcp4-change" ]]; then
  systemctl restart "$svc" || true
elif [[ "$state" == "down" || "$state" == "degraded" || "$state" == "off" ]]; then
  systemctl stop "$svc" || true
fi

if systemctl list-unit-files | grep -q '^mpquic-routing\.service'; then
  systemctl restart mpquic-routing.service || true
fi

exit 0
